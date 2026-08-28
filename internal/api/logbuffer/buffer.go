package logbuffer

import (
	"strings"
	"sync"
	"time"
)

// LogEntry is a single structured log line.
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	Message   string    `json:"message"`
}

// DetectLogLevel classifies a log line as ERROR, WARN, DEBUG, or INFO based
// on the presence of well-known level keywords in the line.
func DetectLogLevel(line string) string {
	upper := strings.ToUpper(line)
	switch {
	case strings.Contains(upper, "ERROR") || strings.Contains(upper, "SEVERE"):
		return "ERROR"
	case strings.Contains(upper, "WARN"):
		return "WARN"
	case strings.Contains(upper, "DEBUG"):
		return "DEBUG"
	default:
		return "INFO"
	}
}

// LogBuffer stores per-controller log entries in ring buffers.
type LogBuffer struct {
	mu       sync.Mutex
	entries  map[string]*ringBuf
	capacity int
}

// New creates a new LogBuffer with the given per-controller capacity.
func New(capacity int) *LogBuffer {
	if capacity <= 0 {
		capacity = 500
	}
	return &LogBuffer{
		entries:  make(map[string]*ringBuf),
		capacity: capacity,
	}
}

// Append adds a log entry for the given controller key ("ns/name").
func (lb *LogBuffer) Append(key string, entry LogEntry) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	rb, ok := lb.entries[key]
	if !ok {
		rb = newRingBuf(lb.capacity)
		lb.entries[key] = rb
	}
	rb.append(entry)
}

// Since returns all log entries for the given key that are newer than the
// given time. If after is zero, returns all entries.
func (lb *LogBuffer) Since(key string, after time.Time) []LogEntry {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	rb, ok := lb.entries[key]
	if !ok {
		return nil
	}
	all := rb.all()
	if after.IsZero() {
		return all
	}
	// Find first entry after the given time.
	for i, e := range all {
		if e.Timestamp.After(after) {
			return all[i:]
		}
	}
	return nil
}

// ringBuf is a fixed-capacity ring buffer of LogEntry.
type ringBuf struct {
	buf      []LogEntry
	head     int
	size     int
	capacity int
}

func newRingBuf(capacity int) *ringBuf {
	return &ringBuf{
		buf:      make([]LogEntry, capacity),
		capacity: capacity,
	}
}

func (rb *ringBuf) append(e LogEntry) {
	if rb.size < rb.capacity {
		rb.buf[(rb.head+rb.size)%rb.capacity] = e
		rb.size++
	} else {
		rb.buf[rb.head] = e
		rb.head = (rb.head + 1) % rb.capacity
	}
}

func (rb *ringBuf) all() []LogEntry {
	out := make([]LogEntry, rb.size)
	for i := 0; i < rb.size; i++ {
		out[i] = rb.buf[(rb.head+i)%rb.capacity]
	}
	return out
}
