package recipe

// expand.go — `${path}` expansion against the runtime's variable Memory.
//
// Supported path syntax (jsonshape-compatible subset):
//
//	${var}                     → Memory["var"]
//	${var.field}               → map["field"]
//	${var.a.b.c}               → nested map walk
//	${var[0]}                  → array element
//	${var[0].field}            → array element then field
//	${var.list[2].name}        → arbitrary mix
//
// Out-of-range / missing-field is a hard error (stop the recipe). Silent
// null would make recipe debugging painful — the operator wants to know
// exactly which path didn't resolve.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// expandToken expands every `${...}` reference inside a single argv token
// against memory. Scalars (string/number/bool) are stringified; arrays and
// objects are emitted as JSON-ish strings via fmt %v (caller may pass the
// result to a subprocess that re-parses).
//
// A token may contain multiple references and surrounding literal text:
//
//	"--nodes=${nodes[0].id}"   → "--nodes=0xabc..."
//	"hello ${greeting} world"   → "hello 안녕 world"
//
// Unclosed `${` (missing `}`) is a parse-time error returned here.
func expandToken(tok string, memory map[string]any) (string, error) {
	if !strings.Contains(tok, "${") {
		return tok, nil
	}
	var out strings.Builder
	i := 0
	for i < len(tok) {
		if i+1 < len(tok) && tok[i] == '$' && tok[i+1] == '{' {
			end := strings.IndexByte(tok[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated `${` in token %q", tok)
			}
			path := tok[i+2 : i+2+end]
			val, err := evalPath(memory, path)
			if err != nil {
				return "", err
			}
			out.WriteString(stringify(val))
			i = i + 2 + end + 1 // past the closing `}`
			continue
		}
		out.WriteByte(tok[i])
		i++
	}
	return out.String(), nil
}

// expandArgv expands every token in argv. Returns the new slice + first error.
func expandArgv(argv []string, memory map[string]any) ([]string, error) {
	out := make([]string, len(argv))
	for i, t := range argv {
		v, err := expandToken(t, memory)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// evalPath walks `path` against memory and returns the resolved value.
// `path` examples: "nodes", "nodes.length", "nodes[0]", "nodes[0].id".
//
// Errors:
//   - empty path
//   - unknown root variable
//   - field missing on object
//   - index out of range on array
//   - field access on a non-object / non-array
//   - malformed token (e.g. unmatched `[`)
func evalPath(memory map[string]any, path string) (any, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("empty path")
	}
	// Tokenize into segments: identifier or [index]. We walk left-to-right.
	// First segment must be a variable name; subsequent `.field` / `[i]` chain.
	root, rest, err := splitFirstIdent(path)
	if err != nil {
		return nil, err
	}
	// M21 — `env` is a reserved namespace for OS environment variables.
	// `${env.HOME}` → os.Getenv("HOME"). Memory["env"] is shadowed (env
	// always wins) so the rule is unambiguous: `env.` always means OS.
	// Unset env var returns empty string (matches os.Getenv semantics).
	if root == "env" {
		if !strings.HasPrefix(rest, ".") {
			return nil, fmt.Errorf("in ${%s}: env requires .NAME (e.g. ${env.HOME})", path)
		}
		name, next, err := splitFirstIdent(rest[1:])
		if err != nil {
			return nil, fmt.Errorf("in ${%s}: %w", path, err)
		}
		if next != "" {
			return nil, fmt.Errorf("in ${%s}: env.%s: unexpected continuation %q (env values are flat strings, no nested fields)", path, name, next)
		}
		return os.Getenv(name), nil
	}
	val, ok := memory[root]
	if !ok {
		return nil, fmt.Errorf("unknown variable %q in ${%s}", root, path)
	}
	for rest != "" {
		switch rest[0] {
		case '.':
			name, next, err := splitFirstIdent(rest[1:])
			if err != nil {
				return nil, fmt.Errorf("in ${%s}: %w", path, err)
			}
			// `length` is a synthetic field on arrays + strings, mirrored after
			// JavaScript / Lua semantics; operators reach for it often when
			// they `echo "found ${nodes.length} nodes"`.
			if name == "length" {
				switch v := val.(type) {
				case []any:
					val = int64(len(v))
				case string:
					val = int64(len(v))
				case nil:
					return nil, fmt.Errorf("in ${%s}: null has no length", path)
				default:
					return nil, fmt.Errorf("in ${%s}: cannot take length of %T", path, val)
				}
			} else {
				obj, ok := val.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("in ${%s}: cannot access .%s on %T", path, name, val)
				}
				v, ok := obj[name]
				if !ok {
					return nil, fmt.Errorf("in ${%s}: field %q not found", path, name)
				}
				val = v
			}
			rest = next
		case '[':
			closeBr := strings.IndexByte(rest, ']')
			if closeBr < 0 {
				return nil, fmt.Errorf("in ${%s}: unmatched `[`", path)
			}
			idxStr := strings.TrimSpace(rest[1:closeBr])
			idx, err := strconv.Atoi(idxStr)
			if err != nil {
				return nil, fmt.Errorf("in ${%s}: invalid index %q", path, idxStr)
			}
			arr, ok := val.([]any)
			if !ok {
				return nil, fmt.Errorf("in ${%s}: cannot index %T", path, val)
			}
			if idx < 0 || idx >= len(arr) {
				return nil, fmt.Errorf("in ${%s}: index %d out of range (len=%d)", path, idx, len(arr))
			}
			val = arr[idx]
			rest = rest[closeBr+1:]
		default:
			return nil, fmt.Errorf("in ${%s}: unexpected character %q", path, rest[0])
		}
	}
	return val, nil
}

// splitFirstIdent reads a leading identifier from s and returns (ident, rest).
// Errors when s does not start with a valid identifier char.
func splitFirstIdent(s string) (string, string, error) {
	if s == "" {
		return "", "", fmt.Errorf("expected identifier")
	}
	end := 0
	for end < len(s) {
		c := s[end]
		isFirst := end == 0
		switch {
		case c == '_':
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case !isFirst && c >= '0' && c <= '9':
		default:
			if end == 0 {
				return "", "", fmt.Errorf("expected identifier, got %q", c)
			}
			return s[:end], s[end:], nil
		}
		end++
	}
	return s, "", nil
}

// stringify formats an evaluated path value for substitution into an argv
// token. Strings pass through; numbers and bools use Go's default %v;
// arrays / objects fall back to %v (compact list / map form). Operators
// who need clean JSON should `${var}` (whole value) on its own as a token
// — the runtime then JSON-marshals it before subprocess injection.
//
// Future: when a token equals exactly "${var}" (no surrounding literal)
// we could marshal it as JSON for downstream `-json` consumers. v1.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		// Print integer-valued floats without a ".0" suffix so `echo ${nodes.length}`
		// shows `3` not `3.0` (json.Unmarshal returns float64 for all numbers).
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}
