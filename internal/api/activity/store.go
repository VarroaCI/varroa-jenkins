package activity

import (
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Event represents a single activity feed entry.
//
// Cluster names the cluster the event is *about*, which is not always the one
// publishing it: the BFF serves every cluster in the brood. Leave it empty and
// Publisher.Publish defaults it to the publisher's own identity.
type Event struct {
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"`
	Source      string    `json:"source"`          // "mite", "operator", "user", "jenkins", "api", "mcp"
	Actor       string    `json:"actor,omitempty"` // username, else email, else subject (api.ActorFrom); "operator"; or empty
	Controller  string    `json:"controller,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Cluster     string    `json:"cluster"`
	Message     string    `json:"message"`
	Phase       string    `json:"phase,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Severity    string    `json:"severity"`
	ItemPath    string    `json:"itemPath,omitempty"`
	BuildNumber int64     `json:"buildNumber,omitempty"`
	Result      string    `json:"result,omitempty"`
	URL         string    `json:"url,omitempty"`
}

// Store is an in-memory ring buffer of activity events with SSE subscriber support.
type Store struct {
	mu       sync.Mutex
	events   []Event
	head     int
	size     int
	capacity int
	subs     []chan Event

	Logger *slog.Logger
}

// New creates a new Store with the given capacity.
func New(capacity int) *Store {
	if capacity <= 0 {
		capacity = 200
	}
	return &Store{
		events:   make([]Event, capacity),
		capacity: capacity,
	}
}

// Append adds an event to the store and notifies SSE subscribers.
func (s *Store) Append(e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	e.Normalize()
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.size < s.capacity {
		s.events[(s.head+s.size)%s.capacity] = e
		s.size++
	} else {
		s.events[s.head] = e
		s.head = (s.head + 1) % s.capacity
	}

	// Notify SSE subscribers.
	for _, ch := range s.subs {
		select {
		case ch <- e:
		default:
			// drop if subscriber is slow
		}
	}
}

var errorSeverityTokens = []string{"failed", "failure", "error", "denied", "unhealthy", "disconnected"}
var warningSeverityTokens = []string{"warning", "retry", "drift", "pending", "paused", "hibernated"}

// Normalize applies the canonical wire representation to producer and retained events.
func (e *Event) Normalize() {
	e.Actor = strings.TrimSpace(e.Actor)
	e.Severity = strings.ToLower(strings.TrimSpace(e.Severity))
	if e.Severity == "info" || e.Severity == "warning" || e.Severity == "error" {
		return
	}
	text := strings.ToLower(e.Phase + " " + e.Reason + " " + e.Type)
	e.Severity = "info"
	for _, token := range warningSeverityTokens {
		if strings.Contains(text, token) {
			e.Severity = "warning"
			break
		}
	}
	for _, token := range errorSeverityTokens {
		if strings.Contains(text, token) {
			e.Severity = "error"
			break
		}
	}
}

// List returns all stored events, optionally filtered by controller name.
func (s *Store) List(controller string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Event
	for i := 0; i < s.size; i++ {
		e := s.events[(s.head+i)%s.capacity]
		if controller != "" && e.Controller != controller {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Subscribe returns a channel that receives new events as they are appended.
func (s *Store) Subscribe() <-chan Event {
	ch := make(chan Event, 64)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, ch)
	return ch
}

// Unsubscribe removes a subscriber.
func (s *Store) Unsubscribe(ch <-chan Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sub := range s.subs {
		if sub == ch {
			s.subs = append(s.subs[:i], s.subs[i+1:]...)
			close(sub)
			break
		}
	}
}

// Notify is the generic method for emitting any activity event.
func (s *Store) Notify(e Event) {
	s.Append(e)
}

// NotifyConnect is a convenience for emitting a "connected" event.
func (s *Store) NotifyConnect(name, namespace, version string) {
	s.Notify(Event{
		Type:       "connected",
		Source:     "mite",
		Controller: name,
		Namespace:  namespace,
		Message:    "mite " + version + " connected",
	})
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("mite connected", "namespace", namespace, "name", name, "version", version)
}

// NotifyDisconnect is a convenience for emitting a "disconnected" event.
func (s *Store) NotifyDisconnect(name, namespace string) {
	s.Notify(Event{
		Type:       "disconnected",
		Source:     "mite",
		Controller: name,
		Namespace:  namespace,
		Message:    "mite disconnected",
	})
}

// NotifyPhase is a convenience for emitting a "phase" transition event.
func (s *Store) NotifyPhase(name, namespace, phase string) {
	s.Notify(Event{
		Type:       "phase",
		Source:     "mite",
		Controller: name,
		Namespace:  namespace,
		Message:    "phase changed to " + phase,
	})
}
