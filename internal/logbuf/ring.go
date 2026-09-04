package logbuf

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Ring is a fixed-size log ring for TUI display.
type Ring struct {
	mu   sync.RWMutex
	size int
	lines []string
}

func New(size int) *Ring {
	if size <= 0 {
		size = 50
	}
	return &Ring{size: size}
}

func (r *Ring) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg == "" {
		return len(p), nil
	}
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		r.mu.Lock()
		r.lines = append(r.lines, fmt.Sprintf("%s %s", time.Now().Format("15:04:05"), line))
		if len(r.lines) > r.size {
			r.lines = r.lines[len(r.lines)-r.size:]
		}
		r.mu.Unlock()
	}
	return len(p), nil
}

func (r *Ring) Lines(n int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n <= 0 || n >= len(r.lines) {
		out := make([]string, len(r.lines))
		copy(out, r.lines)
		return out
	}
	out := make([]string, n)
	copy(out, r.lines[len(r.lines)-n:])
	return out
}
