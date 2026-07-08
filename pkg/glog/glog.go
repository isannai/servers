package glog

import (
	"io"
	"log"
	"os"
	"sync"
)

// Category represents a log category.
type Category string

const (
	Lifecycle  Category = "lifecycle"
	Connection Category = "connection"
	Request    Category = "request"
	Error      Category = "error"
	Debug      Category = "debug"
)

var allCategories = []Category{Lifecycle, Connection, Request, Error, Debug}

// Config holds log output and filtering settings.
type Config struct {
	Output     string   `json:"output"`      // "console" | "file" | "both" | "disabled" (default: "console")
	File       string   `json:"file"`        // file path when output includes file
	Rotate     string   `json:"rotate"`      // "daily" | "" (default: no rotation)
	MaxFiles   int      `json:"max_files"`   // max rotated files to keep (default: 10)
	Categories []string `json:"categories"`  // enabled categories (empty = all)
}

// Logger provides category-filtered logging with configurable output.
type Logger struct {
	enabled map[Category]bool
	buf     *RingBuffer
	mu      sync.RWMutex
}

// New creates a Logger with the given config.
// Sets up log output target and category filter.
func New(cfg Config) *Logger {
	l := &Logger{
		enabled: make(map[Category]bool),
		buf:     NewRingBuffer(1000),
	}

	// Category filter
	if len(cfg.Categories) == 0 {
		for _, c := range allCategories {
			l.enabled[c] = true
		}
	} else {
		for _, c := range cfg.Categories {
			l.enabled[Category(c)] = true
		}
	}

	// Build file writer if needed
	var fileWriter io.Writer
	if cfg.File != "" {
		if cfg.Rotate == "daily" {
			maxFiles := cfg.MaxFiles
			if maxFiles <= 0 {
				maxFiles = 10
			}
			fileWriter = NewDailyRotateWriter(cfg.File, maxFiles)
		} else {
			f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
			if err == nil {
				fileWriter = f
			}
		}
	}

	// Output target
	var writers []io.Writer
	writers = append(writers, l.buf) // always write to ring buffer (admin API)

	// Windows console 은 bare \n 만 받으면 cursor 가 column 0 으로 reset
	// 되지 않아 다음 라인이 이전 라인 끝 위치에서 시작 (들여쓰기 누적).
	// wrapConsole 이 platform 에 따라 \n → \r\n 변환 (windows) 또는
	// pass-through (linux/mac).
	switch cfg.Output {
	case "both":
		writers = append(writers, wrapConsole(os.Stderr))
		if fileWriter != nil {
			writers = append(writers, fileWriter)
		}
	case "file":
		if fileWriter != nil {
			writers = append(writers, fileWriter)
		}
	case "disabled":
		// ring buffer only
	default: // "console" or empty
		writers = append(writers, wrapConsole(os.Stderr))
	}

	log.SetOutput(io.MultiWriter(writers...))
	return l
}

// Log prints a formatted log message if the category is enabled.
func (l *Logger) Log(cat Category, format string, args ...any) {
	l.mu.RLock()
	ok := l.enabled[cat]
	l.mu.RUnlock()
	if ok {
		log.Printf(format, args...)
	}
}

// Buffer returns the ring buffer for admin API / SSE subscriptions.
func (l *Logger) Buffer() *RingBuffer {
	return l.buf
}
