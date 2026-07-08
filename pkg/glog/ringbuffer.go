package glog

import (
	"sync"
)

// RingBuffer is a thread-safe circular buffer that stores recent log lines.
type RingBuffer struct {
	mu     sync.RWMutex
	lines  []string
	pos    int
	count  int
	subs   []chan string
	subsMu sync.Mutex
}

// NewRingBuffer creates a new ring buffer with the given capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{
		lines: make([]string, capacity),
	}
}

// Write implements io.Writer.
func (b *RingBuffer) Write(p []byte) (int, error) {
	line := string(p)
	b.mu.Lock()
	b.lines[b.pos] = line
	b.pos = (b.pos + 1) % len(b.lines)
	if b.count < len(b.lines) {
		b.count++
	}
	b.mu.Unlock()

	b.subsMu.Lock()
	for _, ch := range b.subs {
		select {
		case ch <- line:
		default:
		}
	}
	b.subsMu.Unlock()

	return len(p), nil
}

// Recent returns the last n log lines in order.
func (b *RingBuffer) Recent(n int) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if n > b.count {
		n = b.count
	}
	result := make([]string, n)
	start := (b.pos - n + len(b.lines)) % len(b.lines)
	for i := 0; i < n; i++ {
		result[i] = b.lines[(start+i)%len(b.lines)]
	}
	return result
}

// Subscribe returns a channel that receives new log lines.
func (b *RingBuffer) Subscribe() chan string {
	ch := make(chan string, 64)
	b.subsMu.Lock()
	b.subs = append(b.subs, ch)
	b.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (b *RingBuffer) Unsubscribe(ch chan string) {
	b.subsMu.Lock()
	for i, c := range b.subs {
		if c == ch {
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			break
		}
	}
	b.subsMu.Unlock()
	close(ch)
}
