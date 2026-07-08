package recipe

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isannai/isann-servers/pkg/setup"
)

func TestParse_Basic(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\ndocker stop --except sd;\ndocker create sd;\n")
	rc, err := Parse("basic.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.Requires != nil {
		t.Errorf("expected no requires, got %+v", rc.Requires)
	}
	want := [][]string{
		{"docker", "stop", "--except", "sd"},
		{"docker", "create", "sd"},
	}
	if len(rc.Statements) != len(want) {
		t.Fatalf("got %d statements, want %d", len(rc.Statements), len(want))
	}
	for i, w := range want {
		if !reflect.DeepEqual(rc.Statements[i].Argv, w) {
			t.Errorf("stmt %d argv = %v, want %v", i, rc.Statements[i].Argv, w)
		}
	}
}

func TestParse_Requires(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\nrequires:\n  vram: 8G+\n  gpu: RTX 30 | GTX 16\n\nprofile use sd default;\n")
	rc, err := Parse("req.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.Requires == nil {
		t.Fatal("expected requires block")
	}
	if rc.Requires.VRAMGB != 8 {
		t.Errorf("vram = %v, want 8", rc.Requires.VRAMGB)
	}
	if !reflect.DeepEqual(rc.Requires.GPUPatterns, []string{"RTX 30", "GTX 16"}) {
		t.Errorf("gpu patterns = %v", rc.Requires.GPUPatterns)
	}
	if len(rc.Statements) != 1 || rc.Statements[0].Argv[0] != "profile" {
		t.Errorf("statements = %+v", rc.Statements)
	}
}

func TestParse_CommentRequires(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\n# requires:\n#   vram: 4G+\n\ndocker create sd;\n")
	rc, err := Parse("c.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.Requires == nil || rc.Requires.VRAMGB != 4 {
		t.Fatalf("requires = %+v", rc.Requires)
	}
	if len(rc.Statements) != 1 {
		t.Errorf("statements = %+v", rc.Statements)
	}
}

func TestParse_LeadingCommentsThenRequires(t *testing.T) {
	// Regression: leading comment lines before `requires:` must not cause the
	// requires block to be parsed as a statement.
	src := []byte("#pragma ISANN 0.1.20\n# header comment\n# another\n\nrequires:\n  vram: 4G+\n\ndocker create sd;\n")
	rc, err := Parse("lc.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.Requires == nil || rc.Requires.VRAMGB != 4 {
		t.Fatalf("requires = %+v", rc.Requires)
	}
	if len(rc.Statements) != 1 || !reflect.DeepEqual(rc.Statements[0].Argv, []string{"docker", "create", "sd"}) {
		t.Errorf("statements = %+v", rc.Statements)
	}
}

func TestParse_Multiline(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\nmodel pull civitai://x\n  --engine sd\n  --name dream;\n")
	rc, err := Parse("ml.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rc.Statements) != 1 {
		t.Fatalf("want 1 statement, got %d", len(rc.Statements))
	}
	want := []string{"model", "pull", "civitai://x", "--engine", "sd", "--name", "dream"}
	if !reflect.DeepEqual(rc.Statements[0].Argv, want) {
		t.Errorf("argv = %v, want %v", rc.Statements[0].Argv, want)
	}
	if rc.Statements[0].Line != 2 {
		t.Errorf("line = %d, want 2 (pragma at line 1)", rc.Statements[0].Line)
	}
}

func TestParse_Quoted(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\ninfer --prompt \"hello world\" --system 'be brief';\n")
	rc, err := Parse("q.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"infer", "--prompt", "hello world", "--system", "be brief"}
	if !reflect.DeepEqual(rc.Statements[0].Argv, want) {
		t.Errorf("argv = %v, want %v", rc.Statements[0].Argv, want)
	}
}

func TestParse_CommentStripped(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\n# a leading comment\ndocker create sd; # tail comment\n# trailing\n")
	rc, err := Parse("cm.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rc.Statements) != 1 || !reflect.DeepEqual(rc.Statements[0].Argv, []string{"docker", "create", "sd"}) {
		t.Errorf("statements = %+v", rc.Statements)
	}
}

func TestParse_Unterminated(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\ndocker create sd\n") // no ';'
	_, err := Parse("u.ian", src)
	if err == nil {
		t.Fatal("expected unterminated-statement error")
	}
}

func TestParse_UnknownRequiresIsV2(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\nrequires:\n  os: windows\n\ndocker create sd;\n")
	_, err := Parse("os.ian", src)
	if err == nil {
		t.Fatal("expected error for v2 'os' requires")
	}
}

func TestParseVRAM(t *testing.T) {
	cases := map[string]float64{"8G+": 8, "8G": 8, "24GB": 24, "16": 16, "7.5G+": 7.5}
	for in, want := range cases {
		got, err := parseVRAM(in)
		if err != nil || got != want {
			t.Errorf("parseVRAM(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseVRAM("lots"); err == nil {
		t.Error("expected error for invalid vram")
	}
}

func TestRequiresCheck_VRAM(t *testing.T) {
	r := &Requires{VRAMGB: 8}
	// shortfall
	failed := r.CheckAgainst(setup.HardwareSpec{GPUs: []setup.GPUSpec{{Name: "GTX 1650", VramTotalGB: 4}}})
	if len(failed) != 1 {
		t.Errorf("expected 1 vram failure, got %v", failed)
	}
	// pass
	failed = r.CheckAgainst(setup.HardwareSpec{GPUs: []setup.GPUSpec{{Name: "RTX 3090", VramTotalGB: 24}}})
	if len(failed) != 0 {
		t.Errorf("expected pass, got %v", failed)
	}
}

func TestRequiresCheck_GPU(t *testing.T) {
	r := &Requires{GPUPatterns: []string{"RTX 30", "RTX 40"}}
	failed := r.CheckAgainst(setup.HardwareSpec{GPUs: []setup.GPUSpec{{Name: "NVIDIA GeForce GTX 1650"}}})
	if len(failed) != 1 {
		t.Errorf("expected gpu mismatch, got %v", failed)
	}
	failed = r.CheckAgainst(setup.HardwareSpec{GPUs: []setup.GPUSpec{{Name: "NVIDIA GeForce RTX 3090"}}})
	if len(failed) != 0 {
		t.Errorf("expected gpu match, got %v", failed)
	}
}

func TestRequiresCheck_NilAndEmpty(t *testing.T) {
	var r *Requires
	if got := r.Check(); got != nil {
		t.Errorf("nil requires should pass, got %v", got)
	}
	empty := &Requires{}
	if got := empty.CheckAgainst(setup.HardwareSpec{}); len(got) != 0 {
		t.Errorf("empty requires should pass, got %v", got)
	}
}

// --- v2: capture + ${expansion} + builtins ---------------------------------

func TestParse_Capture(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\nnodes := list nodes -json;\necho ${nodes.length};\n")
	rc, err := Parse("cap.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rc.Statements) != 2 {
		t.Fatalf("got %d statements", len(rc.Statements))
	}
	if rc.Statements[0].Capture != "nodes" {
		t.Errorf("stmt[0].Capture = %q, want %q", rc.Statements[0].Capture, "nodes")
	}
	if !reflect.DeepEqual(rc.Statements[0].Argv, []string{"list", "nodes", "-json"}) {
		t.Errorf("stmt[0].Argv = %v", rc.Statements[0].Argv)
	}
	if rc.Statements[1].Capture != "" {
		t.Errorf("stmt[1] should not capture")
	}
	if rc.Statements[1].Argv[0] != "echo" || rc.Statements[1].Argv[1] != "${nodes.length}" {
		t.Errorf("stmt[1].Argv = %v (${} should be preserved raw)", rc.Statements[1].Argv)
	}
}

func TestEvalPath(t *testing.T) {
	mem := map[string]any{
		"name":    "llama",
		"count":   float64(3),
		"running": true,
		"nodes": []any{
			map[string]any{"id": "0xabc", "vram": float64(24)},
			map[string]any{"id": "0xdef", "vram": float64(16)},
		},
		"deep": map[string]any{
			"inner": map[string]any{"value": "nested"},
		},
	}
	cases := []struct {
		path string
		want any
		err  bool
	}{
		{"name", "llama", false},
		{"count", float64(3), false},
		{"running", true, false},
		{"nodes[0].id", "0xabc", false},
		{"nodes[1].vram", float64(16), false},
		{"nodes.length", int64(2), false},
		{"deep.inner.value", "nested", false},
		{"missing", nil, true},          // unknown root
		{"nodes[5].id", nil, true},      // out of range
		{"name.field", nil, true},       // field on string
		{"nodes[0].missing", nil, true}, // missing field
	}
	for _, tc := range cases {
		got, err := evalPath(mem, tc.path)
		if tc.err {
			if err == nil {
				t.Errorf("evalPath(%q) expected error, got %v", tc.path, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("evalPath(%q) error: %v", tc.path, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("evalPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestExpandToken(t *testing.T) {
	mem := map[string]any{
		"engine": "llama",
		"port":   float64(7862),
		"nodes":  []any{map[string]any{"id": "0xabc"}},
	}
	cases := []struct {
		in, want string
		err      bool
	}{
		{"plain", "plain", false},
		{"${engine}", "llama", false},
		{"--engine=${engine}", "--engine=llama", false},
		{"port ${port} active", "port 7862 active", false},
		{"--node=${nodes[0].id}", "--node=0xabc", false},
		{"${engine}-${port}", "llama-7862", false},
		{"${unterminated", "", true},
		{"${missing}", "", true},
	}
	for _, tc := range cases {
		got, err := expandToken(tc.in, mem)
		if tc.err {
			if err == nil {
				t.Errorf("expandToken(%q) expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("expandToken(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("expandToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsVarName(t *testing.T) {
	cases := map[string]bool{
		"nodes":     true,
		"_x":        true,
		"camelCase": true,
		"snake_42":  true,
		"":          false,
		"42nodes":   false, // can't start with digit
		"a-b":       false, // hyphen
		"a.b":       false, // dot
		"${var}":    false,
	}
	for s, want := range cases {
		if got := isVarName(s); got != want {
			t.Errorf("isVarName(%q) = %v, want %v", s, got, want)
		}
	}
}

// --- M21: env namespace in ${path} -----------------------------------------

func TestEvalPath_EnvNamespace(t *testing.T) {
	t.Setenv("ISANN_RECIPE_TEST_VAR", "hello-env")
	t.Setenv("ISANN_RECIPE_TEST_EMPTY", "")

	mem := map[string]any{
		// `env` Memory variable must be shadowed by the env namespace.
		"env":   map[string]any{"trap": "should-not-win"},
		"other": "ok",
	}
	cases := []struct {
		path    string
		want    any
		wantErr bool
	}{
		{"env.ISANN_RECIPE_TEST_VAR", "hello-env", false},
		{"env.ISANN_RECIPE_TEST_EMPTY", "", false},
		{"env.UNSET_VAR_XYZ", "", false}, // unset → empty string (os.Getenv semantics)
		{"other", "ok", false},
		{"env", nil, true},            // bare `env` is invalid — need .NAME
		{"env.HOME.extra", nil, true}, // env values are flat — no continuation
	}
	for _, tc := range cases {
		got, err := evalPath(mem, tc.path)
		if tc.wantErr {
			if err == nil {
				t.Errorf("evalPath(%q): expected error, got %v", tc.path, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("evalPath(%q) error: %v", tc.path, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("evalPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestExpandToken_EnvInterpolation(t *testing.T) {
	t.Setenv("ISANN_RECIPE_TEST_USER", "daesob")
	got, err := expandToken("hi ${env.ISANN_RECIPE_TEST_USER}!", map[string]any{})
	if err != nil {
		t.Fatalf("expandToken: %v", err)
	}
	if got != "hi daesob!" {
		t.Errorf("got %q, want %q", got, "hi daesob!")
	}
}

// --- builtins: `func read` (function) + bare statements --------------------

// The recipe language splits builtins into FUNCTIONS (capturable, reached via
// the `func` namespace — only `read`) and STATEMENTS (bare — echo / sleep /
// var / require / assert). This pins the dispatch rules; the `func read` stdin
// happy-path is exercised by the integration smoke recipes.
func TestExecStatement_Builtins(t *testing.T) {
	rt := &Runtime{Memory: map[string]any{}}

	// `var name = value` — bare statement (the renamed `set`).
	if err := rt.execStatement(Statement{}, []string{"var", "x", "=", "42"}); err != nil {
		t.Fatalf("var: %v", err)
	}
	if rt.Memory["x"] != "42" {
		t.Errorf("Memory[x] = %v, want 42", rt.Memory["x"])
	}

	mustErr := func(name string, st Statement, argv []string, want string) {
		t.Run(name, func(t *testing.T) {
			err := rt.execStatement(st, argv)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("expected error containing %q, got %v", want, err)
			}
		})
	}

	// `func` is the function namespace — it accepts ONLY `read`. Everything
	// else (other builtins, real namespaces, typos) is "not a function".
	mustErr("func no name", Statement{}, []string{"func"}, "missing function name")
	mustErr("func echo", Statement{}, []string{"func", "echo", "hi"}, "not a function")
	mustErr("func set", Statement{}, []string{"func", "set", "x", "=", "1"}, "not a function")
	mustErr("func docker", Statement{}, []string{"func", "docker", "ps"}, "not a function")

	// Builtins are never bare — the error points at the right form.
	mustErr("bare read", Statement{}, []string{"read", "x"}, "func read")
	mustErr("bare set (renamed)", Statement{}, []string{"set", "x", "=", "1"}, "renamed to `var`")

	// Statements have no return value — capturing them is an error.
	for _, name := range []string{"echo", "sleep", "var", "require", "assert"} {
		mustErr("capture "+name, Statement{Capture: "x"}, []string{name, "anything"}, "captured")
	}
}

// --- `#pragma ISANN` directive (M-pragma) ---------------------------------

func TestParsePragma_OK(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20\necho hi;\n")
	rc, err := Parse("p.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.MinIsannVersion != "0.1.20" {
		t.Errorf("MinIsannVersion = %q, want 0.1.20", rc.MinIsannVersion)
	}
}

func TestParsePragma_InlineComment(t *testing.T) {
	src := []byte("#pragma ISANN 0.1.20  # 최소 isann 버전\necho hi;\n")
	rc, err := Parse("pi.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.MinIsannVersion != "0.1.20" {
		t.Errorf("MinIsannVersion = %q, want 0.1.20", rc.MinIsannVersion)
	}
}

func TestParsePragma_Missing(t *testing.T) {
	cases := map[string][]byte{
		"empty file":            []byte(""),
		"statement first":       []byte("echo hi;\n"),
		"blank line first":      []byte("\n#pragma ISANN 0.1.20\necho hi;\n"),
		"comment before pragma": []byte("# note\n#pragma ISANN 0.1.20\necho hi;\n"),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("m.ian", src)
			if err == nil {
				t.Errorf("expected error, got nil")
			} else if !strings.Contains(err.Error(), "pragma") && !strings.Contains(err.Error(), "first line") {
				t.Errorf("error %q should mention pragma / first line", err)
			}
		})
	}
}

func TestParsePragma_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"wrong keyword": []byte("#pragma FOO 0.1.0\necho hi;\n"),
		"no version":    []byte("#pragma ISANN\necho hi;\n"),
		"bad semver":    []byte("#pragma ISANN abc\necho hi;\n"),
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("mal.ian", src)
			if err == nil {
				t.Errorf("expected error, got nil")
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.1.0", 1},
		{"0.1.0", "0.1.1", -1},
		{"1.0.0", "0.9.9", 1},
		{"0.1", "0.1.0", 0},    // missing component = 0
		{"v0.1.0", "0.1.0", 0}, // tolerate `v` prefix
	}
	for _, tc := range cases {
		if got := CompareSemver(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- recipe namespace block (M14/M15/M17 guard) ----------------------------

// `recipe rm` / `recipe list` / `recipe pull` / `recipe exec` must not be
// callable from inside a recipe — they are operator-driven, and allowing
// them inside scripts opens chain attacks (pull) and data loss (rm). The
// runtime returns an explicit error before any dispatch.
func TestExecStatement_BlocksRecipeNamespace(t *testing.T) {
	cases := [][]string{
		{"recipe", "rm", "foo"},
		{"recipe", "ls"},
		{"recipe", "pull", "https://x/y.ian"},
		{"recipe", "exec", "child.ian"},
		{"recipe", "info", "x.ian"},
		{"recipe"}, // bare keyword
	}
	rt := &Runtime{Memory: map[string]any{}}
	for _, argv := range cases {
		err := rt.execStatement(Statement{}, argv)
		if err == nil {
			t.Errorf("argv %v: expected error, got nil", argv)
			continue
		}
		if !strings.Contains(err.Error(), "recipe") {
			t.Errorf("argv %v: error %q should mention `recipe`", argv, err)
		}
	}
}

// --- M13: doc-string extraction --------------------------------------------

func TestParseDocString(t *testing.T) {
	src := []byte(`#pragma ISANN 0.1.20
# name: llama jarvis
# author: daesob
# description: SD + llama bootstrap
# version: 1.2.0

# free-form prose comment (no colon) — should be ignored

requires:
  vram: 8G+

docker create llama;
`)
	rc, err := Parse("ds.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]string{
		"name":        "llama jarvis",
		"author":      "daesob",
		"description": "SD + llama bootstrap",
		"version":     "1.2.0",
	}
	if !reflect.DeepEqual(rc.Meta, want) {
		t.Errorf("Meta = %v, want %v", rc.Meta, want)
	}
}

func TestParseDocString_StopsAtFirstStatement(t *testing.T) {
	// Doc-string scan must NOT pick up `# tag: x` AFTER the first real
	// statement. Only leading comments count.
	src := []byte(`#pragma ISANN 0.1.20
# name: foo
docker create llama;
# tag: late
`)
	rc, err := Parse("ds2.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rc.Meta["name"] != "foo" {
		t.Errorf("Meta[name] = %q, want %q", rc.Meta["name"], "foo")
	}
	if _, ok := rc.Meta["tag"]; ok {
		t.Errorf("Meta[tag] should not be captured after first statement")
	}
}

func TestParseDocString_IgnoresRequiresHeader(t *testing.T) {
	// `# requires:` is a structural header — must not be captured as
	// doc-string entry. (Empty value would already filter it, but be
	// explicit so a future refactor can't reintroduce the bug.)
	src := []byte(`#pragma ISANN 0.1.20
# name: x
# requires:
#   vram: 4G+

docker create llama;
`)
	rc, err := Parse("ds3.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := rc.Meta["requires"]; ok {
		t.Errorf("requires must not appear in Meta: %v", rc.Meta)
	}
	if rc.Meta["name"] != "x" {
		t.Errorf("Meta[name] = %q, want %q", rc.Meta["name"], "x")
	}
}

func TestIsDocKey(t *testing.T) {
	cases := map[string]bool{
		"name":                  true,
		"author":                true,
		"description":           true,
		"my-tag":                true,
		"my_tag":                true,
		"tag42":                 true,
		"":                      false,
		"42name":                false, // digit start
		"name space":            false, // space
		"name.x":                false, // dot
		"name/x":                false, // slash
		strings.Repeat("a", 33): false, // > 32 chars
	}
	for k, want := range cases {
		if got := isDocKey(k); got != want {
			t.Errorf("isDocKey(%q) = %v, want %v", k, got, want)
		}
	}
}

// --- M11: var builtin (formerly set) ---------------------------------------

func TestExecVar(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantKey string
		wantVal any
		wantErr bool
	}{
		{
			name:    "simple",
			argv:    []string{"var", "ttl", "=", "60s"},
			wantKey: "ttl",
			wantVal: "60s",
		},
		{
			name:    "multi-token joined with single space",
			argv:    []string{"var", "msg", "=", "hello", "world"},
			wantKey: "msg",
			wantVal: "hello world",
		},
		{
			name:    "underscore name",
			argv:    []string{"var", "_node_id", "=", "0xabc"},
			wantKey: "_node_id",
			wantVal: "0xabc",
		},
		{
			name:    "value with dashes / colons preserved",
			argv:    []string{"var", "image", "=", "ghcr.io/isannai/llama:latest"},
			wantKey: "image",
			wantVal: "ghcr.io/isannai/llama:latest",
		},
		{
			name:    "missing value",
			argv:    []string{"var", "x", "="},
			wantErr: true,
		},
		{
			name:    "missing = sign",
			argv:    []string{"var", "x", "value"},
			wantErr: true,
		},
		{
			name:    "invalid name (digit start)",
			argv:    []string{"var", "42name", "=", "v"},
			wantErr: true,
		},
		{
			name:    "invalid name (dash)",
			argv:    []string{"var", "a-b", "=", "v"},
			wantErr: true,
		},
		{
			name:    "only var keyword",
			argv:    []string{"var"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &Runtime{Memory: map[string]any{}}
			err := rt.execVar(Statement{}, tc.argv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got Memory=%v", rt.Memory)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got, ok := rt.Memory[tc.wantKey]
			if !ok {
				t.Fatalf("Memory[%q] not set; Memory=%v", tc.wantKey, rt.Memory)
			}
			if got != tc.wantVal {
				t.Errorf("Memory[%q] = %v, want %v", tc.wantKey, got, tc.wantVal)
			}
		})
	}
}

func TestStringify(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{true, "true"},
		{false, "false"},
		{int64(42), "42"},
		{float64(3), "3"}, // integer-valued float drops ".0"
		{float64(3.14), "3.14"},
	}
	for _, tc := range cases {
		if got := stringify(tc.in); got != tc.want {
			t.Errorf("stringify(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- M12: include ----------------------------------------------------------

func TestInclude(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "frag.ian")
	if err := os.WriteFile(frag, []byte("#pragma ISANN 0.1.20\necho a;\necho b;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "main.ian")
	if err := os.WriteFile(main, []byte("#pragma ISANN 0.1.20\necho start;\ninclude \"frag.ian\";\necho end;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(main)
	rc, err := Parse(main, data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := [][]string{{"echo", "start"}, {"echo", "a"}, {"echo", "b"}, {"echo", "end"}}
	if len(rc.Statements) != len(want) {
		t.Fatalf("got %d statements, want %d", len(rc.Statements), len(want))
	}
	for i, w := range want {
		if !reflect.DeepEqual(rc.Statements[i].Argv, w) {
			t.Errorf("stmt %d argv = %v, want %v", i, rc.Statements[i].Argv, w)
		}
	}
	// included statements carry the fragment's path as Src (for error reporting)
	absFrag, _ := filepath.Abs(frag)
	if rc.Statements[1].Src != absFrag {
		t.Errorf("included stmt Src = %q, want %q", rc.Statements[1].Src, absFrag)
	}
}

func TestIncludeCycle(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.ian")
	b := filepath.Join(dir, "b.ian")
	os.WriteFile(a, []byte("#pragma ISANN 0.1.20\necho a;\ninclude \"b.ian\";\n"), 0o644)
	os.WriteFile(b, []byte("#pragma ISANN 0.1.20\necho b;\ninclude \"a.ian\";\n"), 0o644)
	data, _ := os.ReadFile(a)
	if _, err := Parse(a, data); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected include-cycle error, got %v", err)
	}
}

func TestIncludeRequiresRejected(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "frag.ian")
	os.WriteFile(frag, []byte("#pragma ISANN 0.1.20\nrequires:\n  vram: 8G+\n\necho a;\n"), 0o644)
	main := filepath.Join(dir, "main.ian")
	os.WriteFile(main, []byte("#pragma ISANN 0.1.20\ninclude \"frag.ian\";\n"), 0o644)
	data, _ := os.ReadFile(main)
	if _, err := Parse(main, data); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("expected included-requires rejection, got %v", err)
	}
}

// --- M13: require / assert -------------------------------------------------

func TestEvalAssertExpr(t *testing.T) {
	cases := []struct {
		expr    []string
		want    bool
		wantErr bool
	}{
		{[]string{"ready"}, true, false},
		{[]string{""}, false, false},
		{[]string{"false"}, false, false},
		{[]string{"0"}, false, false},
		{[]string{"ready", "==", "ready"}, true, false},
		{[]string{"ready", "==", "nope"}, false, false},
		{[]string{"a", "!=", "b"}, true, false},
		{[]string{"a", "!=", "a"}, false, false},
		{[]string{"a", "<", "b"}, false, true},
		{[]string{"a", "b"}, false, true},
		{[]string{}, false, true},
	}
	for _, tc := range cases {
		got, err := evalAssertExpr(tc.expr)
		if tc.wantErr {
			if err == nil {
				t.Errorf("evalAssertExpr(%v) expected error", tc.expr)
			}
			continue
		}
		if err != nil {
			t.Errorf("evalAssertExpr(%v) unexpected error: %v", tc.expr, err)
		}
		if got != tc.want {
			t.Errorf("evalAssertExpr(%v) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestSplitRequireMessage(t *testing.T) {
	// comma attached to a token: `== ready,` "msg"
	expr, msg := splitRequireMessage([]string{"val", "==", "ready,", "x must be ready"})
	if !reflect.DeepEqual(expr, []string{"val", "==", "ready"}) || msg != "x must be ready" {
		t.Errorf("attached comma: expr=%v msg=%q", expr, msg)
	}
	// lone comma token
	expr, msg = splitRequireMessage([]string{"a", "==", "b", ",", "boom"})
	if !reflect.DeepEqual(expr, []string{"a", "==", "b"}) || msg != "boom" {
		t.Errorf("lone comma: expr=%v msg=%q", expr, msg)
	}
	// no comma → no message
	expr, msg = splitRequireMessage([]string{"a", "==", "b"})
	if !reflect.DeepEqual(expr, []string{"a", "==", "b"}) || msg != "" {
		t.Errorf("no comma: expr=%v msg=%q", expr, msg)
	}
	// comma FIRST — the operand expanded to empty (`require ${env.UNSET}, "msg"`
	// → `require , "msg"`). Hand back a single empty operand [""] (falsy), not
	// [], so require fails with the message instead of "missing expression".
	expr, msg = splitRequireMessage([]string{",", "X required"})
	if !reflect.DeepEqual(expr, []string{""}) || msg != "X required" {
		t.Errorf("comma-first (empty operand): expr=%v msg=%q", expr, msg)
	}
}

// --- control flow (IF / ELSE IF / ELSE / END) --------------------------------

// runForMemory parses + runs src and returns the runtime so a test can inspect
// Memory. Branch bodies use the `var` builtin (no fork/dispatcher needed) so we
// can observe which branch actually executed.
func runForMemory(t *testing.T, src string, seed map[string]any) *Runtime {
	t.Helper()
	rc, err := Parse("t.ian", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rt := &Runtime{Memory: map[string]any{}}
	for k, v := range seed {
		rt.Memory[k] = v
	}
	if err := rt.Run(rc); err != nil {
		t.Fatalf("run: %v", err)
	}
	return rt
}

func TestControlFlow_Parse(t *testing.T) {
	src := "#pragma ISANN 0.1.0\n" +
		"IF ${os} == windows BEGIN\n  echo a;\n" +
		"ELSE IF ${os} == linux BEGIN\n  echo b;\n" +
		"ELSE BEGIN\n  echo c;\n" +
		"END\n"
	rc, err := Parse("cf.ian", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []struct {
		control string
		cond    []string
		argv    []string
	}{
		{"if", []string{"${os}", "==", "windows"}, nil},
		{"", nil, []string{"echo", "a"}},
		{"elseif", []string{"${os}", "==", "linux"}, nil},
		{"", nil, []string{"echo", "b"}},
		{"else", nil, nil},
		{"", nil, []string{"echo", "c"}},
		{"end", nil, nil},
	}
	if len(rc.Statements) != len(want) {
		t.Fatalf("got %d statements, want %d: %+v", len(rc.Statements), len(want), rc.Statements)
	}
	for i, w := range want {
		st := rc.Statements[i]
		if st.Control != w.control {
			t.Errorf("stmt %d control = %q, want %q", i, st.Control, w.control)
		}
		if w.cond != nil && !reflect.DeepEqual(st.Cond, w.cond) {
			t.Errorf("stmt %d cond = %v, want %v", i, st.Cond, w.cond)
		}
		if w.argv != nil && !reflect.DeepEqual(st.Argv, w.argv) {
			t.Errorf("stmt %d argv = %v, want %v", i, st.Argv, w.argv)
		}
	}
}

func TestControlFlow_BranchSelection(t *testing.T) {
	// Nested ${mem.hardware.os} mirrors the real `mem := info -json` mesh case.
	src := "#pragma ISANN 0.1.0\n" +
		"IF ${mem.hardware.os} == windows BEGIN\n  var hit = win;\n" +
		"ELSE IF ${mem.hardware.os} == linux BEGIN\n  var hit = lin;\n" +
		"ELSE BEGIN\n  var hit = other;\n" +
		"END\n"
	for _, tc := range []struct {
		os, want string
	}{
		{"windows", "win"},
		{"linux", "lin"},
		{"darwin", "other"},
	} {
		seed := map[string]any{"mem": map[string]any{"hardware": map[string]any{"os": tc.os}}}
		rt := runForMemory(t, src, seed)
		if got := rt.Memory["hit"]; got != tc.want {
			t.Errorf("os=%s: hit = %v, want %q", tc.os, got, tc.want)
		}
	}
}

func TestControlFlow_NoMatchNoElse(t *testing.T) {
	// No branch matches and there is no ELSE → nothing runs, no error.
	src := "#pragma ISANN 0.1.0\n" +
		"IF ${os} == windows BEGIN\n  var hit = win;\n" +
		"ELSE IF ${os} == linux BEGIN\n  var hit = lin;\n" +
		"END\n"
	rt := runForMemory(t, src, map[string]any{"os": "darwin"})
	if v, ok := rt.Memory["hit"]; ok {
		t.Errorf("expected no branch to run, but hit = %v", v)
	}
}

func TestControlFlow_SkippedBranchNotRun(t *testing.T) {
	// A var set in a non-taken branch must NOT land in Memory.
	src := "#pragma ISANN 0.1.0\n" +
		"IF ${os} == linux BEGIN\n  var lin = 1;\n" +
		"ELSE BEGIN\n  var other = 1;\n" +
		"END\n"
	rt := runForMemory(t, src, map[string]any{"os": "linux"})
	if rt.Memory["lin"] != "1" {
		t.Errorf("taken branch: lin = %v, want 1", rt.Memory["lin"])
	}
	if _, ok := rt.Memory["other"]; ok {
		t.Error("skipped ELSE branch should not have set `other`")
	}
}

func TestControlFlow_Nested(t *testing.T) {
	src := "#pragma ISANN 0.1.0\n" +
		"IF ${os} == linux BEGIN\n" +
		"  IF ${arch} == arm64 BEGIN\n    var hit = linux_arm;\n" +
		"  ELSE BEGIN\n    var hit = linux_other;\n  END\n" +
		"END\n"
	rt := runForMemory(t, src, map[string]any{"os": "linux", "arch": "arm64"})
	if rt.Memory["hit"] != "linux_arm" {
		t.Errorf("nested: hit = %v, want linux_arm", rt.Memory["hit"])
	}

	// Outer not taken → inner must be skipped (and its condition never evaluated).
	rt2 := runForMemory(t, src, map[string]any{"os": "windows", "arch": "arm64"})
	if v, ok := rt2.Memory["hit"]; ok {
		t.Errorf("outer not taken: inner ran (hit=%v)", v)
	}
}

func TestControlFlow_ParseErrors(t *testing.T) {
	cases := map[string]string{
		"END without IF":        "#pragma ISANN 0.1.0\nEND\n",
		"unclosed IF":           "#pragma ISANN 0.1.0\nIF ${x} == 1 BEGIN\n  echo hi;\n",
		"ELSE after ELSE":       "#pragma ISANN 0.1.0\nIF ${x} == 1 BEGIN\nELSE BEGIN\nELSE BEGIN\nEND\n",
		"IF terminated by ;":    "#pragma ISANN 0.1.0\nIF ${x} == 1;\n",
		"BEGIN without IF/ELSE": "#pragma ISANN 0.1.0\ndocker create BEGIN\nEND\n",
		"END ends a statement":  "#pragma ISANN 0.1.0\nIF ${x} == 1 BEGIN\n  echo hi END\n",
		"ELSE IF without cond":  "#pragma ISANN 0.1.0\nIF ${x} == 1 BEGIN\nELSE IF BEGIN\nEND\n",
		"ELSE IF outside IF":    "#pragma ISANN 0.1.0\nELSE IF ${x} == 1 BEGIN\nEND\n",
	}
	for name, src := range cases {
		if _, err := Parse("err.ian", []byte(src)); err == nil {
			t.Errorf("%s: expected a parse error, got nil", name)
		}
	}
}

func TestControlFlow_LiteralBeginEndQuoted(t *testing.T) {
	// BEGIN/END are reserved terminators; to pass them as literal args, quote.
	src := "#pragma ISANN 0.1.0\nvar a = \"BEGIN\";\n"
	rc, err := Parse("q.ian", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rc.Statements) != 1 || rc.Statements[0].Control != "" {
		t.Fatalf("quoted BEGIN should be a normal statement: %+v", rc.Statements)
	}
}

func TestParse_MultilineQuotedString(t *testing.T) {
	// A quote opened on one line and closed on the next keeps the newline literal;
	// the token must NOT split (regression: next line's words used to leak in).
	src := []byte("#pragma ISANN 0.1.0\necho \"line one\n  line two\";\n")
	rc, err := Parse("mq.ian", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rc.Statements) != 1 {
		t.Fatalf("want 1 statement, got %d: %+v", len(rc.Statements), rc.Statements)
	}
	want := []string{"echo", "line one\n  line two"}
	if !reflect.DeepEqual(rc.Statements[0].Argv, want) {
		t.Errorf("argv = %q, want %q", rc.Statements[0].Argv, want)
	}
}

func TestControlFlow_MultilineStringInBranch(t *testing.T) {
	// Regression for the operator's recipe: a multi-line quoted echo inside a
	// branch must not swallow the following ELSE (was: "BEGIN must follow IF...").
	src := "#pragma ISANN 0.1.0\n" +
		"IF ${os} == linux BEGIN\n" +
		"  echo BRANCH_LINUX;\n" +
		"  echo \"--------------\n  test  -----\";\n" +
		"ELSE BEGIN\n  echo OTHER;\n" +
		"END\n"
	rc, err := Parse("mlb.ian", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// if / echo / echo / else / echo / end = 6 statements
	if len(rc.Statements) != 6 {
		t.Fatalf("want 6 statements, got %d: %+v", len(rc.Statements), rc.Statements)
	}
	// the linux branch (2 echos incl. the multi-line one) runs; ELSE does not.
	rt := runForMemory(t, src, map[string]any{"os": "linux"})
	_ = rt
}
