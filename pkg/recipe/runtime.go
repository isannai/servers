package recipe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/term"
)

// Runtime executes a parsed Recipe's statements sequentially.
//
// Each statement is:
//  1. argv tokens expanded against Memory (`${var.x}` → value)
//  2. if Capture is set → stdout buffered + JSON-parsed → Memory[Capture]
//  3. else → stdout/stderr pass straight through
//  4. builtins (echo / sleep / set) intercepted first
//
// Dispatch: by default the runtime invokes InProcessDispatcher (set by main
// at init time, walks rootCmd in-process). If unset OR if ForkOnly is true,
// the runtime falls back to forking the current binary (rt.Self). In-process
// is ~30× faster (no fork overhead) and lets Memory capture pull native Go
// values straight from leaves; subprocess fallback is the safety net for
// commands the dispatcher cannot handle.
//
// timeout is intentionally never set — Stage 4 of Phase 2 wired up unbounded
// HTTP timeouts so multi-GB model pulls don't get killed mid-download. The
// runner inherits that posture; operators kill stuck recipes with Ctrl+C.
type Runtime struct {
	Self      string         // absolute path to the isann binary (os.Executable()) — used by fork fallback
	KeepGoing bool           // continue after a failed statement (report at end)
	DryRun    bool           // print the plan, execute nothing
	Force     bool           // propagate -force into member `<noun> pull` (app/skill/model) so an already-installed member re-pulls instead of skipping
	ForkOnly  bool           // force subprocess fork even when InProcessDispatcher is set (`-fork` flag)
	Trace     bool           // print the per-statement `[N/M] … ✓` trace (OFF by default; the `-debug` flag turns it on). Failures print regardless.
	Memory    map[string]any // captured values keyed by variable name (lazy-allocated)

	// stdinReader is the shared buffered reader for `read` statements (M20).
	// Re-creating it per call drops buffered bytes — `read a; read b;` with
	// piped input would lose `b`'s line. Lazy-init in readStdinLine.
	stdinReader *bufio.Reader
}

// InProcessDispatcher is a hook set by main() at init time. The runtime
// invokes it first; only commands the dispatcher returns `handled=true` for
// run in-process — anything else falls back to forking the binary.
//
// Contract:
//   - Looks up argv[0] (namespace) + argv[1] (verb) in an explicit map; if
//     either is missing returns (false, nil) so the runtime forks.
//   - Recovers dieErr panics into the returned error (handled=true) so a
//     failing leaf does not kill the recipe process. Other panics propagate.
//   - stdout/stderr redirection (capture mode) is set up by the runtime
//     BEFORE calling this — the dispatcher itself does not redirect.
var InProcessDispatcher func(argv []string) (handled bool, err error)

// forcePullSafe lists the pull namespaces whose `pull` verb accepts -force, so
// Runtime.Force can safely inject it (preset/profile/recipe pull have no force
// flag, so they are excluded).
var forcePullSafe = map[string]bool{"app": true, "skill": true, "model": true}

func argvHasForce(argv []string) bool {
	for _, a := range argv {
		if a == "-force" || a == "--force" {
			return true
		}
	}
	return false
}

// Run executes every statement. On failure it stops (default) or, with
// KeepGoing, records the failure and continues, returning a combined error
// at the end. DryRun prints the plan and returns nil.
func (rt *Runtime) Run(rc *Recipe) error {
	if rt.Memory == nil {
		rt.Memory = make(map[string]any)
	}
	n := len(rc.Statements)
	if rt.DryRun {
		fmt.Fprintln(os.Stderr, "[plan]")
		for i, st := range rc.Statements {
			if st.Control != "" {
				fmt.Fprintf(os.Stderr, "  [%d/%d] %s\n", i+1, n, controlRepr(st))
				continue
			}
			prefix := ""
			if st.Capture != "" {
				prefix = st.Capture + " := "
			}
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s%s\n", i+1, n, prefix, strings.Join(st.Argv, " "))
		}
		return nil
	}

	var failures []string
	var condStack []condFrame
	for i, st := range rc.Statements {
		// Control markers (IF/ELSE IF/ELSE/END) drive the branch stack; they
		// don't execute. Conditions are evaluated only in a taken context, so a
		// skipped branch never errors on an unresolved `${...}`.
		if st.Control != "" {
			if err := rt.applyControl(st, &condStack); err != nil {
				if !rt.KeepGoing {
					return err
				}
				failures = append(failures, err.Error())
			}
			continue
		}
		// A normal statement inside a non-taken branch is skipped silently.
		if !condActive(condStack) {
			continue
		}
		// Expand `${path}` references in argv tokens BEFORE deciding builtin
		// vs subprocess so the displayed `[N/M]` marker reflects what actually
		// runs (less mystery for the operator).
		argv, err := expandArgv(st.Argv, rt.Memory)
		if err != nil {
			where := fmt.Sprintf("%s:%d: %v", stmtSrc(st, rc), st.Line, err)
			if !rt.KeepGoing {
				return fmt.Errorf("%s", where)
			}
			failures = append(failures, where)
			continue
		}

		// -force propagation (`app pull <ref> -force` etc.): re-pull an
		// already-installed member instead of skipping. Only for pull verbs
		// that accept -force (app/skill/model).
		if rt.Force && len(argv) >= 2 && argv[1] == "pull" && forcePullSafe[argv[0]] && !argvHasForce(argv) {
			argv = append(argv, "-force")
		}

		displayPrefix := ""
		if st.Capture != "" {
			displayPrefix = st.Capture + " := "
		}
		cmdline := displayPrefix + strings.Join(argv, " ")
		if rt.Trace {
			fmt.Fprintf(os.Stderr, "[%d/%d] %s ...\n", i+1, n, cmdline)
		}

		start := time.Now()
		runErr := rt.execStatement(st, argv)
		dur := time.Since(start).Round(time.Millisecond)

		if runErr != nil {
			// Failures always print, even in quiet mode.
			fmt.Fprintf(os.Stderr, "[%d/%d] ✗ %v (%s)\n", i+1, n, runErr, dur)
			where := fmt.Sprintf("%s:%d: %s — %v", stmtSrc(st, rc), st.Line, cmdline, runErr)
			if !rt.KeepGoing {
				return fmt.Errorf("%s", where)
			}
			failures = append(failures, where)
			continue
		}
		if rt.Trace {
			fmt.Fprintf(os.Stderr, "[%d/%d] ✓ %s\n", i+1, n, dur)
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d statement(s) failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return nil
}

// --- control flow (IF / ELSE IF / ELSE / END) ---------------------------------
//
// The runtime walks the flat statement list keeping a stack of condFrames — one
// per open IF. Each frame remembers whether the surrounding context was taken
// (enclosingActive), whether any branch matched yet (anyMatched), and whether
// the current branch is the taken one (branchActive). A normal statement runs
// iff the innermost frame is branchActive (or the stack is empty). Conditions
// are evaluated lazily — only while the enclosing context is taken — so a
// skipped branch never touches an unresolved `${...}`.

type condFrame struct {
	enclosingActive bool // was the context around this IF executing?
	anyMatched      bool // has some branch in this IF matched already?
	branchActive    bool // is the current branch the taken one?
}

// condActive reports whether statements at the current depth should run.
func condActive(stack []condFrame) bool {
	if len(stack) == 0 {
		return true
	}
	return stack[len(stack)-1].branchActive
}

// applyControl advances the branch stack for one control marker.
func (rt *Runtime) applyControl(st Statement, stack *[]condFrame) error {
	switch st.Control {
	case "if":
		enclosing := condActive(*stack)
		matched := false
		if enclosing { // evaluate the condition only in a taken context
			ok, err := rt.evalCond(st)
			if err != nil {
				return err
			}
			matched = ok
		}
		*stack = append(*stack, condFrame{enclosingActive: enclosing, anyMatched: matched, branchActive: matched})
	case "elseif":
		if len(*stack) == 0 {
			return fmt.Errorf("%s:%d: ELSE IF without IF", st.Src, st.Line)
		}
		f := &(*stack)[len(*stack)-1]
		f.branchActive = false
		if f.enclosingActive && !f.anyMatched {
			ok, err := rt.evalCond(st)
			if err != nil {
				return err
			}
			if ok {
				f.branchActive = true
				f.anyMatched = true
			}
		}
	case "else":
		if len(*stack) == 0 {
			return fmt.Errorf("%s:%d: ELSE without IF", st.Src, st.Line)
		}
		f := &(*stack)[len(*stack)-1]
		f.branchActive = f.enclosingActive && !f.anyMatched
		if f.branchActive {
			f.anyMatched = true
		}
	case "end":
		if len(*stack) == 0 {
			return fmt.Errorf("%s:%d: END without IF", st.Src, st.Line)
		}
		*stack = (*stack)[:len(*stack)-1]
	}
	return nil
}

// evalCond expands the condition's `${...}` against Memory and evaluates it with
// the same comparator the `require`/`assert` builtins use (`a == b`, `a != b`,
// or single-operand truthiness).
func (rt *Runtime) evalCond(st Statement) (bool, error) {
	expanded, err := expandArgv(st.Cond, rt.Memory)
	if err != nil {
		return false, fmt.Errorf("%s:%d: %s condition: %w", st.Src, st.Line, ctrlWord(st.Control), err)
	}
	ok, err := evalAssertExpr(expanded)
	if err != nil {
		return false, fmt.Errorf("%s:%d: %s condition: %w", st.Src, st.Line, ctrlWord(st.Control), err)
	}
	return ok, nil
}

// controlRepr renders a control marker for the dry-run plan.
func controlRepr(st Statement) string {
	switch st.Control {
	case "if":
		return "IF " + strings.Join(st.Cond, " ") + " BEGIN"
	case "elseif":
		return "ELSE IF " + strings.Join(st.Cond, " ") + " BEGIN"
	case "else":
		return "ELSE BEGIN"
	case "end":
		return "END"
	}
	return ""
}

// execStatement runs one statement (expanded argv). Builtins (echo / sleep /
// set) short-circuit first; capture mode buffers stdout for JSON parse;
// default mode runs in-process (if InProcessDispatcher is set) or forks the
// binary as a fallback.
func (rt *Runtime) execStatement(st Statement, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty statement")
	}

	// Hard block — recipe-management commands cannot run from inside a
	// recipe. Rationale:
	//   - `recipe exec X`  : child-recipe call is M19 (deferred); use M11
	//                        `include` to compose statements at parse time.
	//   - `recipe pull`    : a recipe pulling another recipe is a chain
	//                        attack vector (malicious recipe fetches more).
	//   - `recipe rm`/`ls` : data destruction / no useful capture surface;
	//                        recipe-management belongs in the host shell.
	// All four are operator-driven workflows, not script primitives.
	if argv[0] == "recipe" {
		verb := ""
		if len(argv) >= 2 {
			verb = argv[1]
		}
		return fmt.Errorf("`recipe %s` cannot run from inside a recipe (recipe-management commands are host-shell only; use `include` to compose statements)", verb)
	}
	// `market install` from inside a recipe (install.ian) is a chain-attack vector
	// — a malicious install.ian fetching and running MORE installs — same rationale
	// as `recipe pull`. install.ian fetches its own files with `app pull`; it must
	// never nest another install.
	if argv[0] == "market" && len(argv) >= 2 && argv[1] == "install" {
		return fmt.Errorf("`market install` cannot run from inside a recipe (chain-attack guard) — use `app pull <url>` to fetch files")
	}

	// `func <name>` is the namespace for FUNCTIONS — builtins that return a
	// value you capture. `read` is the only function today. Capture is OPTIONAL:
	// `user_name := func read "prompt"` stores the line; `func read "press
	// enter"` reads and discards (a pause/confirm). `func` on anything else is
	// an error — echo / sleep / var / require / assert are statements, not
	// functions, and are written bare.
	if argv[0] == "func" {
		if len(argv) < 2 {
			return fmt.Errorf("func: missing function name (functions: read)")
		}
		switch argv[1] {
		case "read":
			return rt.execRead(st, argv[1:])
		default:
			return fmt.Errorf("func: %q is not a function (functions: read)", argv[1])
		}
	}

	// Statements — echo / sleep / var / require / assert. Written bare (no
	// `func`), no return value, so capturing them is an error. `read` is a
	// function (call it `func read`), and `set` was renamed to `var`.
	switch argv[0] {
	case "echo":
		if st.Capture != "" {
			return fmt.Errorf("echo: cannot be captured (no return value)")
		}
		return rt.execEcho(st, argv)
	case "sleep":
		if st.Capture != "" {
			return fmt.Errorf("sleep: cannot be captured (no return value)")
		}
		return rt.execSleep(st, argv)
	case "var":
		if st.Capture != "" {
			return fmt.Errorf("var: cannot be captured — `var name = value` is a constant; use `name := <command>` to capture command output")
		}
		return rt.execVar(st, argv)
	case "require", "assert":
		if st.Capture != "" {
			return fmt.Errorf("%s: cannot be captured (no return value)", argv[0])
		}
		return rt.execRequire(argv)
	case "read":
		return fmt.Errorf("read is a function — call it as `func read \"prompt\"` (optionally `name := func read \"prompt\"`)")
	case "set":
		return fmt.Errorf("`set` was renamed to `var` — write `var name = value`")
	}

	// Capture mode — buffer stdout, parse JSON into Memory.
	if st.Capture != "" {
		return rt.execCapture(st, argv)
	}

	// In-process dispatch when main wired the hook AND the command is in
	// the explicit map (dispatcher returns handled=true). Anything else
	// falls through to subprocess fork — keeps the surface area small and
	// dangerous commands (os.Exit-callers, interactive flows) safe.
	if rt.useInProcess() {
		handled, err := InProcessDispatcher(argv)
		if handled {
			return err
		}
	}

	// Fallback — subprocess with stdout/stderr passthrough. Triggered by
	// `-fork` flag, unregistered commands, or contexts that did not wire
	// the hook (e.g. external test binary).
	cmd := exec.Command(rt.Self, argv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// useInProcess reports whether the runtime should invoke InProcessDispatcher
// instead of fork. False when the hook is unset OR ForkOnly is enabled.
func (rt *Runtime) useInProcess() bool {
	return InProcessDispatcher != nil && !rt.ForkOnly
}

// execCapture runs the command with stdout buffered. Stderr passes through
// so the operator still sees progress hints. Stdout is parsed as JSON and
// the result lands in Memory[st.Capture]. `-json` is NOT auto-appended —
// the caller is responsible (every isann query command supports it via
// Phase 3's output contract; recipe operators just add the flag explicitly).
//
// In-process path: temporarily reassigns os.Stdout to a pipe whose read
// end drains into the buffer, calls the dispatcher, then restores it.
// Thread-unsafe by design — recipes run statements sequentially.
//
// Fork path: pipes stdout from the child process to the buffer the normal
// way.
//
// If stdout is not valid JSON the recipe fails — silent string fallback
// would hide schema drift / non-JSON commands, making `${var.field}`
// debugging painful.
func (rt *Runtime) execCapture(st Statement, argv []string) error {
	var stdout bytes.Buffer

	if rt.useInProcess() {
		handled, err := rt.captureInProcess(argv, &stdout)
		if handled {
			if err != nil {
				if stdout.Len() > 0 {
					fmt.Fprintf(os.Stderr, "[capture stdout: %s]\n", strings.TrimSpace(stdout.String()))
				}
				return err
			}
			return rt.storeCapture(st, stdout.Bytes())
		}
		// not handled — fall through to fork
	}

	cmd := exec.Command(rt.Self, argv...)
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		// Surface the captured stdout to aid debugging — without it the operator
		// gets only "exit status 1" and no clue what the command printed.
		if stdout.Len() > 0 {
			fmt.Fprintf(os.Stderr, "[capture stdout: %s]\n", strings.TrimSpace(stdout.String()))
		}
		return err
	}
	return rt.storeCapture(st, stdout.Bytes())
}

// captureInProcess runs the in-process dispatcher with os.Stdout swapped
// for a pipe so the leaf's stdout writes land in `out`. Returns
// (handled=false) when the dispatcher does not own the command — caller
// then falls back to fork. Stderr is left alone so progress hints still
// reach the operator.
//
// Thread-unsafe by design — the swap mutates the process-global
// os.Stdout. Safe because recipe statements run sequentially.
func (rt *Runtime) captureInProcess(argv []string, out *bytes.Buffer) (handled bool, err error) {
	r, w, perr := os.Pipe()
	if perr != nil {
		return false, fmt.Errorf("capture: create pipe: %w", perr)
	}
	saved := os.Stdout
	os.Stdout = w

	// Drain the pipe concurrently — leaf may write more than the pipe
	// buffer (~64KB) and block otherwise.
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(out, r)
		close(done)
	}()

	handled, err = InProcessDispatcher(argv)

	_ = w.Close()
	<-done
	_ = r.Close()
	os.Stdout = saved
	return handled, err
}

// storeCapture parses the buffered stdout as JSON and stores the value in
// Memory[st.Capture]. Empty stdout → nil. Non-JSON → error with preview.
func (rt *Runtime) storeCapture(st Statement, raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		rt.Memory[st.Capture] = nil
		return nil
	}
	var val any
	if err := json.Unmarshal(trimmed, &val); err != nil {
		preview := string(raw)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return fmt.Errorf("capture %q: stdout is not JSON (add -json flag?): %v\n  got: %s",
			st.Capture, err, preview)
	}
	rt.Memory[st.Capture] = val
	return nil
}

// execRead reads one line from stdin. It is reached only via the `func`
// namespace — `func read "prompt"` or `name := func read "prompt"`. Capture is
// OPTIONAL: with `:=` the line lands in Memory[Capture]; without one the line
// is read and discarded (a "press enter to continue" pause). The variable name
// NEVER comes from an argv token — there is no imperative `read var` form.
//
// Why builtin (not subprocess): stdin redirection inside a subprocess
// would re-prompt and break batch flows (`recipe exec < answers.txt`).
// The runtime owns stdin and feeds variables directly.
//
// Output (prompt) goes to stderr — stdout stays clean for capture/pipes.
//
// Secret mode (`func read -secret ...`) uses golang.org/x/term.ReadPassword
// when stdin is a TTY; non-TTY (piped input, CI) silently falls back to a
// plain ReadLine so `cat answers.txt | recipe exec ...` keeps working.
func (rt *Runtime) execRead(st Statement, argv []string) error {
	// argv[0] == "read" (the `func` marker was stripped by the caller).
	i := 1
	secret := false
	if i < len(argv) && argv[i] == "-secret" {
		secret = true
		i++
	}
	if prompt := strings.Join(argv[i:], " "); prompt != "" {
		fmt.Fprint(os.Stderr, prompt)
	}

	line, err := rt.readStdinLine(secret)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if st.Capture != "" {
		rt.Memory[st.Capture] = line // no capture → read + discard
	}
	return nil
}

// readStdinLine reads one line from os.Stdin. secret=true tries the
// terminal-aware echo-suppressed read and falls back to a normal line read
// when stdin is not a TTY (pipes / CI). The trailing newline (echo'd by
// ReadPassword on success) lands on stderr so the prompt's line still
// terminates visually.
func (rt *Runtime) readStdinLine(secret bool) (string, error) {
	if secret && term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		// ReadPassword swallows the trailing CR/LF — print one so the next
		// console line doesn't sit on the prompt.
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	if rt.stdinReader == nil {
		rt.stdinReader = bufio.NewReader(os.Stdin)
	}
	line, err := rt.stdinReader.ReadString('\n')
	// EOF with content (no trailing newline) is a valid one-line input.
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// execVar stores a constant or pre-expanded value into Memory (the `var`
// statement, formerly `set`):
//
//	var ttl = 60s;
//	var msg = "hello world";       # quoted → one token
//	var greet = Hello, ${name};    # expand happens before we get here
//
// Grammar: exactly `var <name> = <value...>` with whitespace around `=`.
// The remaining tokens are joined with single spaces so multi-token values
// (without surrounding quotes) still work. Values are always stored as
// strings — capture (`name := cmd -json`) is the path for native JSON.
//
// Why a statement (not parser-level like capture): assignment is a runtime
// concern, not a statement shape. Keeping it here means the parser stays
// minimal and the same `${}` expansion runs first — so
// `var greet = "Hi, ${name}"` evaluates the expansion before assignment.
func (rt *Runtime) execVar(st Statement, argv []string) error {
	if len(argv) < 4 {
		return fmt.Errorf("var: expected `var <name> = <value>`, got %d args", len(argv)-1)
	}
	name := argv[1]
	if !isVarName(name) {
		return fmt.Errorf("var: invalid name %q (must start with letter/_, then alphanum/_)", name)
	}
	if argv[2] != "=" {
		return fmt.Errorf("var: expected `=` between name and value, got %q", argv[2])
	}
	value := strings.Join(argv[3:], " ")
	rt.Memory[name] = value
	return nil
}

// execEcho prints argv[1:] (already expanded) to stderr. We pick stderr so
// that `recipe exec | jq` style piping doesn't get polluted by recipe-side
// messages — only real command stdout reaches the pipe.
func (rt *Runtime) execEcho(st Statement, argv []string) error {
	_, err := io.WriteString(os.Stderr, strings.Join(argv[1:], " ")+"\n")
	return err
}

// execSleep accepts a single duration argument parseable by time.ParseDuration
// (`10s`, `500ms`, `2m`). No argument = error — silent zero-sleep would mask
// a typo like `sleep ${ttl}` where ttl resolved to empty.
func (rt *Runtime) execSleep(st Statement, argv []string) error {
	if len(argv) != 2 {
		return fmt.Errorf("sleep: expected exactly one duration (e.g. `sleep 10s`), got %d args", len(argv)-1)
	}
	d, err := time.ParseDuration(argv[1])
	if err != nil {
		return fmt.Errorf("sleep: invalid duration %q: %w", argv[1], err)
	}
	time.Sleep(d)
	return nil
}

// stmtSrc returns the file a statement came from (for error messages). Falls
// back to the top recipe path for statements built without a Src.
func stmtSrc(st Statement, rc *Recipe) string {
	if st.Src != "" {
		return st.Src
	}
	return rc.Path
}

// execRequire runs the `require` / `assert` builtin (M13) — a RUNTIME assert
// against already-expanded argv (distinct from parse-time `requires:`):
//
//	require ${x} == "ready", "x must be ready";   # equality + custom message
//	require ${x} != "", "x must be set";           # inequality
//	require ${ready};                              # truthy (non-empty & not false/0/no)
//	assert  ${x} == "ready";                       # same, default message on fail
//
// A failed assertion returns an error → the recipe aborts (unless -keep-going).
// argv[0] is "require" or "assert".
func (rt *Runtime) execRequire(argv []string) error {
	kw := argv[0]
	expr, msg := splitRequireMessage(argv[1:])
	ok, err := evalAssertExpr(expr)
	if err != nil {
		return fmt.Errorf("%s: %v", kw, err)
	}
	if !ok {
		if msg == "" {
			msg = fmt.Sprintf("%s failed: %s", kw, strings.Join(expr, " "))
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// splitRequireMessage splits `<expr...> , <message>` on the first comma (a lone
// `,` token, or a token ending in `,`). No comma → all tokens are the expr and
// the message is empty.
//
// When the comma is the FIRST token, the operand was present but expanded to
// empty — `require ${env.UNSET}, "msg"` collapses to `require , "msg"`. That is
// a falsy value, not a syntax error, so we hand back a single empty operand
// ([""]) and let evalAssertExpr fail with the message instead of reporting
// "missing expression" (which would swallow the operator's custom message).
func splitRequireMessage(tokens []string) (expr []string, msg string) {
	for i, t := range tokens {
		if t == "," {
			if i == 0 {
				return []string{""}, strings.Join(tokens[1:], " ")
			}
			return tokens[:i], strings.Join(tokens[i+1:], " ")
		}
		if strings.HasSuffix(t, ",") {
			expr = append(expr, tokens[:i]...)
			expr = append(expr, strings.TrimSuffix(t, ","))
			return expr, strings.Join(tokens[i+1:], " ")
		}
	}
	return tokens, ""
}

// evalAssertExpr evaluates a require/assert expression. Two forms:
//   - 1 token  → truthiness of the value
//   - 3 tokens → `<a> == <b>` or `<a> != <b>` string comparison
func evalAssertExpr(expr []string) (bool, error) {
	switch len(expr) {
	case 0:
		return false, fmt.Errorf("missing expression")
	case 1:
		return isTruthy(expr[0]), nil
	case 3:
		a, op, b := expr[0], expr[1], expr[2]
		switch op {
		case "==":
			return a == b, nil
		case "!=":
			return a != b, nil
		default:
			return false, fmt.Errorf("unknown operator %q (use == or !=)", op)
		}
	default:
		return false, fmt.Errorf("expr must be `<value>` or `<a> ==|!= <b>`, got %d tokens", len(expr))
	}
}

// isTruthy reports whether a 1-operand assert value is "true". Empty / false /
// 0 / no / off / null / nil (case-insensitive) are false; everything else true.
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "0", "no", "off", "null", "nil":
		return false
	}
	return true
}
