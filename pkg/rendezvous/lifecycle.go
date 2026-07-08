package rendezvous

import (
	"github.com/isannai/isann-servers/pkg/setup"
)

// Lifecycle event handling lives in control_tcp.go's applyServiceLifecycle
// now — that's where the TCP-control frame dispatch is. This file keeps
// only the inspect-payload coercion helper, shared by both lifecycle and
// any future event-aware code paths.

// applyInspectFromEvent extracts inspect / inspect_labels / inspect_order
// fields out of a service_event payload and merges them onto the entry.
// Provider sends these as nested generic maps so we have to coerce types.
func applyInspectFromEvent(entry *setup.ServiceInfo, info map[string]any) {
	if v, ok := info["inspect"].(map[string]any); ok && len(v) > 0 {
		out := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok && s != "" {
				out[k] = s
			}
		}
		if len(out) > 0 {
			entry.Inspect = out
		}
	}
	if v, ok := info["inspect_labels"].(map[string]any); ok && len(v) > 0 {
		out := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok && s != "" {
				out[k] = s
			}
		}
		if len(out) > 0 {
			entry.InspectLabels = out
		}
	}
	if v, ok := info["inspect_order"].([]any); ok && len(v) > 0 {
		out := make([]string, 0, len(v))
		for _, val := range v {
			if s, ok := val.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) > 0 {
			entry.InspectOrder = out
		}
	}
}
