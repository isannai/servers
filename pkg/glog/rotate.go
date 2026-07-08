package glog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DailyRotateWriter implements io.Writer with daily log rotation.
// Log files are rotated at midnight, old files are automatically deleted.
//
// Example:
//
//	logs/sd-api.log               ← today
//	logs/sd-api.2026-03-30.log
//	logs/sd-api.2026-03-29.log
//	...oldest files beyond maxFiles are deleted
type DailyRotateWriter struct {
	mu       sync.Mutex
	file     *os.File
	basePath string // e.g. "logs/sd-api.log"
	curDate  string // e.g. "2026-03-31"
	maxFiles int
}

// NewDailyRotateWriter creates a new DailyRotateWriter.
// basePath is the log file path (e.g. "logs/sd-api.log").
// maxFiles is the maximum number of rotated files to keep.
func NewDailyRotateWriter(basePath string, maxFiles int) *DailyRotateWriter {
	if maxFiles <= 0 {
		maxFiles = 10
	}
	dir := filepath.Dir(basePath)
	if dir != "" && dir != "." {
		os.MkdirAll(dir, 0755)
	}

	w := &DailyRotateWriter{
		basePath: basePath,
		curDate:  time.Now().Format("2006-01-02"),
		maxFiles: maxFiles,
	}

	f, err := os.OpenFile(basePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		// fallback to stderr if file can't be opened
		return w
	}
	w.file = f
	return w
}

// Write implements io.Writer. Checks date on each write and rotates if needed.
func (w *DailyRotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if today != w.curDate {
		w.rotate(today)
	}

	if w.file == nil {
		return len(p), nil // discard if no file
	}
	return w.file.Write(p)
}

// rotate closes the current file, renames it with the date suffix, opens a new file,
// and cleans up old files exceeding maxFiles.
func (w *DailyRotateWriter) rotate(today string) {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}

	// Rename current file: logs/sd-api.log → logs/sd-api.2026-03-30.log
	ext := filepath.Ext(w.basePath)
	base := strings.TrimSuffix(w.basePath, ext)
	rotatedName := fmt.Sprintf("%s.%s%s", base, w.curDate, ext)
	os.Rename(w.basePath, rotatedName)

	// Open new file
	f, err := os.OpenFile(w.basePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		w.file = f
	}
	w.curDate = today

	// Cleanup old files
	w.cleanup(base, ext)
}

// cleanup removes rotated log files exceeding maxFiles.
func (w *DailyRotateWriter) cleanup(base, ext string) {
	dir := filepath.Dir(base)
	if dir == "" {
		dir = "."
	}
	prefix := filepath.Base(base) + "."
	suffix := ext

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	var rotated []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == filepath.Base(w.basePath) {
			continue
		}
		// Match pattern: sd-api.2026-03-30.log
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			rotated = append(rotated, name)
		}
	}

	if len(rotated) <= w.maxFiles {
		return
	}

	// Sort ascending by date (oldest first)
	sort.Strings(rotated)

	// Delete oldest files
	toDelete := len(rotated) - w.maxFiles
	for i := 0; i < toDelete; i++ {
		os.Remove(filepath.Join(dir, rotated[i]))
	}
}
