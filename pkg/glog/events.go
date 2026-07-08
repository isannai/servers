package glog

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EventLogConfig maps to the "event_log" config block in service conf files.
// Shape mirrors the main log block (file / rotate / max_files) so both are
// configured consistently.
//
// Example:
//
//	"event_log": {
//	    "enabled": true,
//	    "file": "logs/events/sd-api.jsonl",
//	    "rotate": "daily",
//	    "max_files": 14
//	}
type EventLogConfig struct {
	Enabled  bool   `json:"enabled"`
	File     string `json:"file"`      // full path to event log file
	Rotate   string `json:"rotate"`    // "daily" | "" (no rotation)
	MaxFiles int    `json:"max_files"`
}

// EventWriter emits structured JSONL events to {dir}/{service}.jsonl
// with daily rotation, plus keeps an in-memory ring buffer for instant
// HTTP read access (/v1/logs).
//
// When cfg.Enabled is false, Emit and Tail are no-ops.
type EventWriter struct {
	enabled bool
	file    io.Writer // *DailyRotateWriter or nil
	buf     *RingBuffer
	service string
	mu      sync.Mutex
}

// eventBufferCap is the in-memory ring buffer size (recent events).
const eventBufferCap = 500

// NewEventWriter returns an EventWriter configured for the given service.
// When cfg.Enabled is false or cfg.File is empty, a disabled writer is returned.
//
// Rotation is controlled by cfg.Rotate:
//   - "daily" → daily rotation with cfg.MaxFiles retention (like the main log block)
//   - ""      → plain append, no rotation (matches legacy log behavior)
func NewEventWriter(cfg EventLogConfig, service string) *EventWriter {
	if !cfg.Enabled || cfg.File == "" || service == "" {
		return &EventWriter{enabled: false}
	}
	maxFiles := cfg.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 14
	}

	// Ensure parent directory exists.
	if dir := filepath.Dir(cfg.File); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	var file io.Writer
	if cfg.Rotate == "daily" {
		file = NewDailyRotateWriter(cfg.File, maxFiles)
	} else {
		// plain append, no rotation (consistent with main log block semantics)
		if f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			file = f
		}
	}

	return &EventWriter{
		enabled: true,
		file:    file,
		buf:     NewRingBuffer(eventBufferCap),
		service: service,
	}
}

// Emit writes a single structured event. The common fields (ts, event,
// service) are auto-added; custom fields come from the fields map.
//
// Output format is one JSON object per line (JSONL):
//
//	{"ts":"2026-04-17T13:00:05.123Z","event":"service.started","service":"sd-api","pid":12345}
func (w *EventWriter) Emit(event string, fields map[string]any) {
	if w == nil || !w.enabled {
		return
	}

	// Build ordered JSON: ts, event, service first, then fields.
	var b bytes.Buffer
	b.WriteByte('{')
	writeKV(&b, "ts", time.Now().UTC().Format(time.RFC3339Nano))
	b.WriteByte(',')
	writeKV(&b, "event", event)
	b.WriteByte(',')
	writeKV(&b, "service", w.service)

	for k, v := range fields {
		if k == "ts" || k == "event" || k == "service" {
			continue // reserved common fields
		}
		b.WriteByte(',')
		keyJSON, err := json.Marshal(k)
		if err != nil {
			continue
		}
		valJSON, err := json.Marshal(v)
		if err != nil {
			continue
		}
		b.Write(keyJSON)
		b.WriteByte(':')
		b.Write(valJSON)
	}
	b.WriteByte('}')
	b.WriteByte('\n')

	line := b.Bytes()

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		_, _ = w.file.Write(line)
	}
	if w.buf != nil {
		// ring buffer stores the line WITHOUT the trailing newline,
		// so Tail() callers can control framing.
		_, _ = w.buf.Write(bytes.TrimRight(line, "\n"))
	}
}

// Tail returns the last n events as raw JSON lines (without trailing newlines).
// When disabled, returns an empty slice.
func (w *EventWriter) Tail(n int) []string {
	if w == nil || !w.enabled || w.buf == nil {
		return nil
	}
	return w.buf.Recent(n)
}

// writeKV writes a JSON `"key":"value"` pair (string value only).
func writeKV(b *bytes.Buffer, key, val string) {
	keyJSON, _ := json.Marshal(key)
	valJSON, _ := json.Marshal(val)
	b.Write(keyJSON)
	b.WriteByte(':')
	b.Write(valJSON)
}
