// Package recipe implements the isann recipe runner — a tiny script
// format for replaying a sequence of isann commands with an optional
// hardware precondition check.
//
// Current scope (see docs/TODO/scripts/isann-cli-recipe.md):
//   - `requires:` block with vram + gpu checks
//   - `;`-terminated statements, each run as `isann <argv...>` subprocess
//   - variable capture (`nodes := list nodes`) + `${path}` expansion
//   - control flow: `IF <cond> BEGIN … ELSE IF … ELSE … END` (BEGIN/END are
//     terminators alongside `;`; cond reuses the require/assert comparator)
//   - builtin commands: echo / sleep
//
// Statements preserve `${path}` references raw; the runtime expands them
// at execution time against the variable Memory.
package recipe

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Recipe is a parsed .ian file.
type Recipe struct {
	Path            string
	MinIsannVersion string            // `#pragma ISANN x.y.z` — required minimum isann CLI version
	Meta            map[string]string // M13 — `# key: value` lines at the top of the file
	Requires        *Requires         // nil = no precheck
	Statements      []Statement
}

// Statement is one parsed unit: either a `;`-terminated command (the argv handed
// to isann) OR a control-flow marker (Control != "").
//
// Capture (v2) — when set to a non-empty variable name (`nodes := list nodes`)
// the runtime captures the subprocess stdout, parses it as JSON, and stores
// the value in Memory[Capture]. Argv tokens preserve `${path}` references
// raw — the runtime expands them at execution time against Memory.
//
// Control flow — `IF <cond> BEGIN … ELSE IF <cond> BEGIN … ELSE BEGIN … END`
// (BEGIN/END are statement terminators alongside `;`). A control marker carries
// no Argv; Control is one of "if"/"elseif"/"else"/"end" and Cond holds the
// condition tokens (raw `${...}` preserved) for if/elseif. The runtime evaluates
// branches in order and runs the first match (see runtime.go).
type Statement struct {
	Line    int      // 1-based line where the statement begins (error reporting)
	Argv    []string // tokens, quotes stripped, raw `${path}` preserved (empty for control markers)
	Capture string   // "" = no capture; otherwise the variable name (`nodes` in `nodes := ...`)
	Src     string   // file the statement came from (for errors; differs from top recipe after `include`)
	Control string   // "" = normal command; else "if"|"elseif"|"else"|"end"
	Cond    []string // condition tokens for Control=="if"/"elseif" (raw `${}` preserved)
}

// Requires is the v0.1 hardware precondition block (vram + gpu only).
type Requires struct {
	VRAMGB      float64  // 0 = no check
	GPUPatterns []string // alternation list; empty = no check
}

// Parse parses a .ian source. path is used only for error messages.
//
// Grammar (v0.1):
//
//	requires:            # or "# requires:"
//	  vram: 8G+          # 2-space indent items, blank line ends the block
//	  gpu: RTX 30 | RTX 40
//
//	docker stop --except sd;   # ;-terminated statements, multiline OK
//	docker create sd;
func Parse(path string, src []byte) (*Recipe, error) {
	rc, err := parseFile(path, src)
	if err != nil {
		return nil, err
	}
	// M12 — resolve `include "x.ian";` by inlining the target's statements
	// (recursive, cycle-checked). Paths resolve relative to each file's dir.
	abs, _ := filepath.Abs(path)
	stmts, err := resolveIncludes(rc.Statements, filepath.Dir(abs), map[string]bool{abs: true})
	if err != nil {
		return nil, err
	}
	rc.Statements = stmts
	return rc, nil
}

// parseFile parses ONE recipe file (no include resolution). Parse wraps it.
//
// Every recipe MUST declare `#pragma ISANN <version>` (typically the first
// non-blank line). The pragma names the minimum isann CLI version required
// — Parse refuses to load a recipe without it, and Parse's caller (in
// recipe.go's cmdRecipeExec) checks the version against the running CLI's
// IsannCliVersion. This is the recipe-format equivalent of a manifest
// version: bumping the CLI without bumping pragma'd recipes would let new
// builtins slip into old recipes silently.
func parseFile(path string, src []byte) (*Recipe, error) {
	rc := &Recipe{Path: path}
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")

	minVer, pragmaErr := parsePragma(path, lines)
	if pragmaErr != nil {
		return nil, pragmaErr
	}
	rc.MinIsannVersion = minVer

	rc.Meta = parseDocString(lines)

	bodyStart, req, err := parseRequires(path, lines)
	if err != nil {
		return nil, err
	}
	rc.Requires = req

	stmts, err := parseStatements(path, lines, bodyStart)
	if err != nil {
		return nil, err
	}
	rc.Statements = stmts
	return rc, nil
}

// parsePragma extracts the mandatory `#pragma ISANN <version>` directive.
// It MUST be literally the first line of the file (no leading blank lines,
// no preceding comments). This keeps the contract simple: a reader can
// glance at the top of any recipe and know immediately which isann CLI
// it targets — no scanning needed.
//
// Inline trailing comment is allowed:
//
//	#pragma ISANN 0.1.0    # 최소 isann 버전
func parsePragma(path string, lines []string) (string, error) {
	if len(lines) == 0 {
		return "", fmt.Errorf("%s: empty recipe — first line must be `#pragma ISANN <version>`", path)
	}
	first := strings.TrimSpace(lines[0])
	if first == "" || !strings.HasPrefix(first, "#pragma") {
		return "", fmt.Errorf("%s:1: first line must be `#pragma ISANN <version>` (e.g. `#pragma ISANN 0.1.0`), got %q", path, lines[0])
	}
	// Strip inline trailing comment ` # ...` (second `#`).
	rest := strings.TrimSpace(strings.TrimPrefix(first, "#pragma"))
	if hash := strings.Index(rest, " #"); hash >= 0 {
		rest = strings.TrimSpace(rest[:hash])
	}
	parts := strings.Fields(rest)
	if len(parts) < 2 || parts[0] != "ISANN" {
		return "", fmt.Errorf("%s:1: malformed pragma %q — expected `#pragma ISANN <version>`", path, first)
	}
	ver := parts[1]
	if !looksLikeSemver(ver) {
		return "", fmt.Errorf("%s:1: pragma version %q is not semver (e.g. `0.1.0`)", path, ver)
	}
	return ver, nil
}

// looksLikeSemver does a cheap shape check — digits.digits.digits with an
// optional leading `v`. We don't allow pre-release / build suffixes today
// because there's no use case yet and rejecting them keeps comparisons
// trivial. CompareSemver below assumes this shape.
func looksLikeSemver(s string) bool {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// CompareSemver returns -1, 0, +1 for a<b, a==b, a>b. Missing components
// are treated as zero (`0.1` == `0.1.0`). Caller is responsible for shape
// validation (see looksLikeSemver). `v` prefix is tolerated for either side.
func CompareSemver(a, b string) int {
	aa := splitVerNums(a)
	bb := splitVerNums(b)
	n := len(aa)
	if len(bb) > n {
		n = len(bb)
	}
	for i := 0; i < n; i++ {
		av := 0
		if i < len(aa) {
			av = aa[i]
		}
		bv := 0
		if i < len(bb) {
			bv = bb[i]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}

func splitVerNums(s string) []int {
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

// parseDocString extracts leading `# key: value` comment lines (M13). Scan
// runs over the very top of the file until the first non-comment line —
// blanks are skipped. Anything that does not match the `# ident: value`
// shape (free-form prose, URLs, `# requires:` header) is ignored.
//
// The keys themselves are free-form (operator may add `# audience:`,
// `# tags:`, etc.). `recipe info` highlights the conventional ones —
// name / author / description / version / license — and shows everything
// else under "Other".
func parseDocString(lines []string) map[string]string {
	out := map[string]string{}
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue // blank line — keep scanning the doc-string block
		}
		if !strings.HasPrefix(t, "#") {
			return out // first real line ends the doc-string scan
		}
		if t == "# requires:" {
			return out // requires header is structural, not metadata
		}
		body := strings.TrimSpace(strings.TrimPrefix(t, "#"))
		if body == "" {
			continue
		}
		key, val, ok := strings.Cut(body, ":")
		if !ok {
			continue // plain comment, no colon
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key == "" || val == "" || !isDocKey(key) {
			continue
		}
		out[key] = val
	}
	return out
}

// isDocKey gates which `# key:` lines are admitted as doc-string entries.
// Keeps the doc-string clean of stray colon-bearing comments like
// `# URL: https://...` (URL is fine; "URL" passes) or arbitrary prose.
// Allowed chars: letters, digits (not first), `_`, `-`. Max 32 chars.
func isDocKey(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || r == '-':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// resolveIncludes walks stmts and replaces every `include "<path>";` statement
// with the (recursively include-resolved) statements of the target file. Paths
// resolve relative to baseDir. `chain` holds the abs paths currently on the
// include stack — a repeat is a cycle. The included file must NOT carry its own
// `requires:` (hardware gating belongs to the top recipe).
func resolveIncludes(stmts []Statement, baseDir string, chain map[string]bool) ([]Statement, error) {
	var out []Statement
	for _, st := range stmts {
		if len(st.Argv) == 0 || st.Argv[0] != "include" {
			out = append(out, st)
			continue
		}
		if len(st.Argv) != 2 {
			return nil, fmt.Errorf("%s:%d: include takes exactly one path: include \"file.ian\"", st.Src, st.Line)
		}
		target := st.Argv[1]
		if !filepath.IsAbs(target) {
			target = filepath.Join(baseDir, target)
		}
		abs, _ := filepath.Abs(target)
		if chain[abs] {
			return nil, fmt.Errorf("%s:%d: include cycle: %s", st.Src, st.Line, abs)
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: include %q: %v", st.Src, st.Line, st.Argv[1], err)
		}
		inc, err := parseFile(abs, data)
		if err != nil {
			return nil, err
		}
		if inc.Requires != nil {
			return nil, fmt.Errorf("%s: included by %s:%d must not have its own `requires:` (put it in the top recipe)", abs, st.Src, st.Line)
		}
		chain[abs] = true
		sub, err := resolveIncludes(inc.Statements, filepath.Dir(abs), chain)
		delete(chain, abs)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
	}
	return out, nil
}

// parseRequires scans for a leading `requires:` (or `# requires:`) block and
// returns the line index where the body begins. When no requires block is
// present the body starts at the first meaningful line and req is nil.
func parseRequires(path string, lines []string) (bodyStart int, req *Requires, err error) {
	i := 0
	// Skip leading blank / pure-comment lines to find the first directive.
	// A `# requires:` line is the comment-form header, so it must be matched
	// before the generic comment skip below.
	for i < len(lines) {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			i++
			continue
		}
		if t == "requires:" || t == "# requires:" {
			break
		}
		if strings.HasPrefix(t, "#") {
			i++ // leading comment line — skip past it to keep seeking
			continue
		}
		// First meaningful (non-comment) line is a statement → no requires block.
		return i, nil, nil
	}
	if i >= len(lines) {
		return i, nil, nil // empty file
	}

	// Consume requires items until a blank line (block terminator).
	req = &Requires{}
	i++ // past the `requires:` header
	for ; i < len(lines); i++ {
		raw := lines[i]
		if strings.TrimSpace(raw) == "" {
			i++ // blank line ends the block; body starts after it
			break
		}
		item := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "#"))
		if item == "" {
			continue // blank-but-for-# comment line inside the block
		}
		key, val, ok := strings.Cut(item, ":")
		if !ok {
			return 0, nil, fmt.Errorf("%s:%d: requires item must be 'key: value': %q", path, i+1, raw)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "vram":
			gb, perr := parseVRAM(val)
			if perr != nil {
				return 0, nil, fmt.Errorf("%s:%d: %v", path, i+1, perr)
			}
			req.VRAMGB = gb
		case "gpu":
			req.GPUPatterns = parseAlternation(val)
		case "os", "docker", "engines_installed", "engines_running":
			return 0, nil, fmt.Errorf("%s:%d: requires %q is v2 — v0.1 supports only vram, gpu", path, i+1, key)
		default:
			return 0, nil, fmt.Errorf("%s:%d: unknown requires key %q (v0.1 supports vram, gpu)", path, i+1, key)
		}
	}
	return i, req, nil
}

// termKind is how a raw token-run is terminated: by `;` (normal statement), or
// by the bare keywords BEGIN / END (control-flow block delimiters).
type termKind int

const (
	termSemi termKind = iota
	termBegin
	termEnd
)

// parseStatements tokenizes the body (lines[start:]) into statements. There are
// THREE statement terminators: `;` ends a normal command; the bare words BEGIN
// and END (reserved) delimit `IF … BEGIN … END` control blocks. Newline is a
// token separator only (multi-line statements are fine). A trailing token run
// with no terminator is an error.
func parseStatements(path string, lines []string, start int) ([]Statement, error) {
	var (
		out       []Statement
		argv      []string
		tok       strings.Builder
		inTok     bool
		tokQuoted bool // current token contained a quoted segment (→ not a BEGIN/END keyword)
		quote     byte // 0 / '"' / '\''
		stmtLine  int
		segErr    error
	)

	// emit converts the accumulated argv + its terminator into 0..1 statements.
	emit := func(term termKind) {
		if segErr != nil {
			return
		}
		switch term {
		case termBegin:
			st, err := headerStatement(path, stmtLine, argv)
			if err != nil {
				segErr = err
			} else {
				out = append(out, st)
			}
		case termEnd:
			if len(argv) > 0 {
				segErr = fmt.Errorf("%s:%d: END cannot terminate a statement (missing ';'?): %q", path, stmtLine, strings.Join(argv, " "))
			} else {
				out = append(out, Statement{Line: stmtLine, Control: "end", Src: path})
			}
		case termSemi:
			if len(argv) > 0 {
				st, err := normalStatement(path, stmtLine, argv)
				if err != nil {
					segErr = err
				} else {
					out = append(out, st)
				}
			}
		}
		argv = nil
	}

	// flushTok completes the current token. BEGIN/END are reserved terminators —
	// completing one of those words emits a control segment instead of appending
	// it to argv. (Quote a literal "BEGIN"/"END" to pass it as an argument.)
	flushTok := func() {
		if !inTok {
			return
		}
		t := tok.String()
		tok.Reset()
		inTok = false
		wasQuoted := tokQuoted
		tokQuoted = false
		// A quoted token (e.g. "BEGIN") is a literal argument, never a keyword.
		if !wasQuoted {
			switch t {
			case "BEGIN":
				emit(termBegin)
				return
			case "END":
				emit(termEnd)
				return
			}
		}
		argv = append(argv, t)
	}
	startTok := func(lineNo int) {
		if !inTok {
			inTok = true
			if len(argv) == 0 {
				stmtLine = lineNo // first token of a new statement / segment
			}
		}
	}

	for li := start; li < len(lines); li++ {
		lineNo := li + 1
		line := lines[li]
		for j := 0; j < len(line); j++ {
			ch := line[j]
			if quote != 0 {
				if ch == quote {
					quote = 0 // close; token continues (so adjacent chars append)
				} else {
					tok.WriteByte(ch)
				}
				continue
			}
			switch ch {
			case '#':
				j = len(line) // rest of line is a comment
			case ' ', '\t':
				flushTok()
			case ';':
				flushTok()
				emit(termSemi)
			case '"', '\'':
				startTok(lineNo)
				tokQuoted = true
				quote = ch
			default:
				startTok(lineNo)
				tok.WriteByte(ch)
			}
			if segErr != nil {
				return nil, segErr
			}
		}
		// End of line. Inside an open quote the string spans the newline (kept as
		// a literal '\n', shell-style) — do NOT flush, or the token would split and
		// the next line's words leak in. Outside a quote the newline just separates
		// tokens.
		if quote != 0 {
			tok.WriteByte('\n')
		} else {
			flushTok()
		}
		if segErr != nil {
			return nil, segErr
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("%s:%d: unterminated quote (opened but never closed)", path, stmtLine)
	}
	flushTok()
	if segErr != nil {
		return nil, segErr
	}
	if len(argv) > 0 {
		return nil, fmt.Errorf("%s:%d: statement not terminated with ';' (or BEGIN/END)", path, stmtLine)
	}
	if err := validateControlFlow(path, out); err != nil {
		return nil, err
	}
	return out, nil
}

// normalStatement builds a `;`-terminated command statement, detecting the
// capture form `name := command args`. A leading IF/ELSE here means the operator
// wrote a header without BEGIN — a clear error.
func normalStatement(path string, line int, argv []string) (Statement, error) {
	if argv[0] == "IF" || argv[0] == "ELSE" {
		return Statement{}, fmt.Errorf("%s:%d: %s must open a block with BEGIN (terminated by ';' instead)", path, line, argv[0])
	}
	// Detect capture form: `name := command args`. Three tokens minimum. The
	// variable name must be a single bare identifier — no quotes, no `${}`.
	capture := ""
	if len(argv) >= 3 && argv[1] == ":=" && isVarName(argv[0]) {
		capture = argv[0]
		argv = argv[2:]
	}
	return Statement{Line: line, Argv: argv, Capture: capture, Src: path}, nil
}

// headerStatement builds the control marker for a BEGIN-terminated header:
// `IF <cond>`, `ELSE IF <cond>`, or `ELSE`.
func headerStatement(path string, line int, argv []string) (Statement, error) {
	if len(argv) == 0 {
		return Statement{}, fmt.Errorf("%s:%d: BEGIN without an IF/ELSE header", path, line)
	}
	switch argv[0] {
	case "IF":
		if len(argv) < 2 {
			return Statement{}, fmt.Errorf("%s:%d: IF needs a condition before BEGIN", path, line)
		}
		return Statement{Line: line, Control: "if", Cond: argv[1:], Src: path}, nil
	case "ELSE":
		if len(argv) == 1 {
			return Statement{Line: line, Control: "else", Src: path}, nil
		}
		if argv[1] == "IF" {
			if len(argv) < 3 {
				return Statement{}, fmt.Errorf("%s:%d: ELSE IF needs a condition before BEGIN", path, line)
			}
			return Statement{Line: line, Control: "elseif", Cond: argv[2:], Src: path}, nil
		}
		return Statement{}, fmt.Errorf("%s:%d: after ELSE expected IF or BEGIN, got %q", path, line, argv[1])
	default:
		return Statement{}, fmt.Errorf("%s:%d: BEGIN must follow IF / ELSE IF / ELSE, got %q", path, line, argv[0])
	}
}

// ctrlWord renders a Control kind for error messages.
func ctrlWord(c string) string {
	switch c {
	case "if":
		return "IF"
	case "elseif":
		return "ELSE IF"
	case "else":
		return "ELSE"
	case "end":
		return "END"
	}
	return c
}

// validateControlFlow checks the control markers form well-balanced blocks:
// every ELSE/ELSE IF sits inside an open IF (and after no prior ELSE), every END
// closes an IF, and no IF is left open at end of file.
func validateControlFlow(path string, stmts []Statement) error {
	type frame struct {
		line    int
		sawElse bool
	}
	var stack []frame
	for _, st := range stmts {
		switch st.Control {
		case "if":
			stack = append(stack, frame{line: st.Line})
		case "elseif", "else":
			if len(stack) == 0 {
				return fmt.Errorf("%s:%d: %s outside an IF block", path, st.Line, ctrlWord(st.Control))
			}
			top := &stack[len(stack)-1]
			if top.sawElse {
				return fmt.Errorf("%s:%d: %s after ELSE in the same IF block", path, st.Line, ctrlWord(st.Control))
			}
			if st.Control == "else" {
				top.sawElse = true
			}
		case "end":
			if len(stack) == 0 {
				return fmt.Errorf("%s:%d: END without a matching IF", path, st.Line)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("%s:%d: IF block not closed with END", path, stack[len(stack)-1].line)
	}
	return nil
}

// parseVRAM parses "8G+", "8G", "24GB", "8" → gigabytes. The trailing '+'
// (">=") and a G/GB unit suffix are optional and stripped.
func parseVRAM(s string) (float64, error) {
	v := strings.TrimSpace(s)
	v = strings.TrimSuffix(v, "+")
	v = strings.TrimSpace(v)
	low := strings.ToLower(v)
	low = strings.TrimSuffix(low, "gb")
	low = strings.TrimSuffix(low, "g")
	low = strings.TrimSpace(low)
	gb, err := strconv.ParseFloat(low, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid vram %q (expected e.g. 8G+)", s)
	}
	return gb, nil
}

// isVarName reports whether s is a valid recipe variable name. The first
// char must be a letter or `_`; subsequent chars may be alphanumeric or `_`.
// Used to gate the `name := command` capture syntax — anything else (quoted,
// containing `${}`, starting with a digit, ...) is treated as a plain argv
// token, not a variable.
func isVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// parseAlternation splits "RTX 30 | RTX 40" into ["RTX 30", "RTX 40"],
// trimming each. Empty entries are dropped.
func parseAlternation(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "|") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
