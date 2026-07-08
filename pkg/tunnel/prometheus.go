package tunnel

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// ParsePrometheus reads Prometheus text exposition format (v0.0.4) and
// returns a flat map of metric_name → aggregated numeric value.
//
// Aggregation depends on the metric's declared `# TYPE`:
//   - counter / histogram / summary  → SUM across label sets (counters split
//     by label such as vllm:request_success_total{finished_reason="..."}
//     must be summed to get the true total).
//   - gauge / untyped / no TYPE line → last value wins (gauges represent an
//     instantaneous value, not something to add up).
//
// Comment and empty lines are skipped. Lines with non-numeric values
// (e.g. "NaN" guarded by stray characters) are silently dropped rather than
// aborting — scraping a healthy endpoint must never fail hard because of
// one bad gauge.
//
// Histogram "_bucket", "_sum", "_count" suffixes inherit the parent's type
// declaration (they are all counters in effect), so they are summed too.
//
// This is a minimal parser tailored to what IANN needs from vLLM. Callers
// that need per-label breakdowns must parse the raw text themselves.
func ParsePrometheus(r io.Reader) (map[string]float64, error) {
	out := make(map[string]float64)
	types := make(map[string]string) // name → "counter"|"gauge"|"histogram"|"summary"
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate long histogram lines

	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		if line[0] == '#' {
			// "# TYPE <name> <kind>"
			if parts := strings.Fields(line); len(parts) >= 4 && parts[1] == "TYPE" {
				types[parts[2]] = parts[3]
			}
			continue
		}

		// Split "<name>{labels}" from "<value> [timestamp]".
		name, rest := splitNameAndRest(line)
		if name == "" {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			continue
		}
		// First whitespace-separated token after the labels is the value.
		fields := strings.Fields(rest)
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}

		// Histogram / summary suffixes inherit the parent's declared type.
		kindKey := name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if strings.HasSuffix(name, suffix) {
				kindKey = strings.TrimSuffix(name, suffix)
				break
			}
		}
		kind := types[kindKey]
		if kind == "" {
			kind = types[name]
		}

		switch kind {
		case "counter", "histogram", "summary":
			out[name] += v
		default:
			// gauge, untyped, or TYPE missing — keep last observed value.
			out[name] = v
		}
	}
	return out, s.Err()
}

// splitNameAndRest returns (metric_name, everything_after_labels). Handles:
//   "foo 1"            → ("foo", "1")
//   "foo{a=\"b\"} 1"   → ("foo", "1")
//   "foo{a=\"}\",b=1}" → ("foo", "1")   ← quoted "}" inside label value
func splitNameAndRest(line string) (string, string) {
	// Name ends at the first '{' or whitespace.
	nameEnd := len(line)
	for i, c := range line {
		if c == '{' || c == ' ' || c == '\t' {
			nameEnd = i
			break
		}
	}
	name := line[:nameEnd]
	rest := line[nameEnd:]

	if strings.HasPrefix(rest, "{") {
		// Walk through label section, respecting quoted strings so that a
		// "}" inside a label value doesn't fool us.
		inQuote := false
		escape := false
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			if escape {
				escape = false
				continue
			}
			switch {
			case c == '\\' && inQuote:
				escape = true
			case c == '"':
				inQuote = !inQuote
			case c == '}' && !inQuote:
				return name, rest[i+1:]
			}
		}
		// Malformed: no closing brace. Treat whole rest as value.
	}
	return name, rest
}
