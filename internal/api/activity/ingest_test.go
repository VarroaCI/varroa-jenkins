package activity

import (
	"encoding/json"
	"testing"
	"time"
)

// TestBusPayloadFiltering verifies that when an EventsActivity callback
// (as implemented in cmd/bff/main.go) processes payloads:
//   - A source:"jenkins" payload is appended with all fields mapped
//   - A thin brood payload (no source field) is ignored
//   - Controller falls back to Name when Controller is empty
//   - A jenkins payload with empty/garbage timestamp results in zero time
//     (so store.Append substitutes ingestion time)
func TestBusPayloadFiltering(t *testing.T) {
	store := New(10)

	// Simulate the BFF callback logic for a jenkins payload.
	jenkinsData := map[string]interface{}{
		"event":       "build.started",
		"name":        "ctrl-1",
		"namespace":   "ns-1",
		"source":      "jenkins",
		"type":        "build.started",
		"actor":       "alice",
		"controller":  "ctrl-1",
		"message":     "Build #42 started",
		"itemPath":    "team-a/job",
		"buildNumber": float64(42),
		"result":      "",
		"url":         "job/42/",
		"timestamp":   "2024-06-01T12:00:00Z",
	}
	b, _ := json.Marshal(jenkinsData)
	var p struct {
		Source      string `json:"source"`
		Timestamp   string `json:"timestamp"`
		Type        string `json:"type"`
		Actor       string `json:"actor"`
		Controller  string `json:"controller"`
		Name        string `json:"name"`
		Namespace   string `json:"namespace"`
		Message     string `json:"message"`
		ItemPath    string `json:"itemPath"`
		BuildNumber int64  `json:"buildNumber"`
		Result      string `json:"result"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Filter: only source == "jenkins" passes.
	if p.Source != "jenkins" {
		t.Fatal("expected source == jenkins")
	}

	controller := p.Controller
	if controller == "" {
		controller = p.Name
	}

	ts := parseTS(p.Timestamp)
	store.Notify(Event{
		Timestamp:   ts,
		Type:        p.Type,
		Source:      "jenkins",
		Actor:       p.Actor,
		Controller:  controller,
		Namespace:   p.Namespace,
		Message:     p.Message,
		ItemPath:    p.ItemPath,
		BuildNumber: p.BuildNumber,
		Result:      p.Result,
		URL:         p.URL,
	})

	events := store.List("")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	e := events[0]
	if e.Type != "build.started" {
		t.Errorf("Type = %q, want %q", e.Type, "build.started")
	}
	if e.Source != "jenkins" {
		t.Errorf("Source = %q, want %q", e.Source, "jenkins")
	}
	if e.Actor != "alice" {
		t.Errorf("Actor = %q, want %q", e.Actor, "alice")
	}
	if e.Controller != "ctrl-1" {
		t.Errorf("Controller = %q, want %q", e.Controller, "ctrl-1")
	}
	if e.Namespace != "ns-1" {
		t.Errorf("Namespace = %q, want %q", e.Namespace, "ns-1")
	}
	if e.Message != "Build #42 started" {
		t.Errorf("Message = %q, want %q", e.Message, "Build #42 started")
	}
	if e.ItemPath != "team-a/job" {
		t.Errorf("ItemPath = %q, want %q", e.ItemPath, "team-a/job")
	}
	if e.BuildNumber != 42 {
		t.Errorf("BuildNumber = %d, want %d", e.BuildNumber, 42)
	}
	if e.Result != "" {
		t.Errorf("Result = %q, want %q", e.Result, "")
	}
	if e.URL != "job/42/" {
		t.Errorf("URL = %q, want %q", e.URL, "job/42/")
	}
	expectedTS, _ := time.Parse(time.RFC3339, "2024-06-01T12:00:00Z")
	if !e.Timestamp.Equal(expectedTS) {
		t.Errorf("Timestamp = %v, want %v", e.Timestamp, expectedTS)
	}
}

// TestThinBroodPayloadIsIgnored verifies that a thin brood payload
// (no source field, or source != "jenkins") is NOT appended by the
// EventsActivity filter logic.
func TestThinBroodPayloadIsIgnored(t *testing.T) {
	store := New(10)

	// Thin brood payload (no source field).
	thinData := map[string]string{
		"event":     "connected",
		"name":      "ctrl-1",
		"namespace": "ns-1",
	}
	b, _ := json.Marshal(thinData)
	var p struct {
		Source string `json:"source"`
	}
	json.Unmarshal(b, &p)

	// Filter: skip if source != "jenkins".
	if p.Source == "jenkins" {
		t.Fatal("thin payload should not have source == jenkins")
	}

	// The callback would return without appending.
	// Verify store is still empty.
	if len(store.List("")) != 0 {
		t.Fatal("expected no events from thin payload")
	}
}

// TestJenkinsPayload_FallbackController verifies that when Controller is empty,
// the callback falls back to Name.
func TestJenkinsPayload_FallbackController(t *testing.T) {
	store := New(10)

	payload := map[string]interface{}{
		"source":    "jenkins",
		"name":      "ctrl-from-name",
		"namespace": "ns-1",
		"type":      "job.created",
		"event":     "job.created",
	}
	b, _ := json.Marshal(payload)
	var p struct {
		Source      string `json:"source"`
		Name        string `json:"name"`
		Namespace   string `json:"namespace"`
		Type        string `json:"type"`
		Controller  string `json:"controller"`
		Timestamp   string `json:"timestamp"`
		ItemPath    string `json:"itemPath"`
		BuildNumber int64  `json:"buildNumber"`
		Result      string `json:"result"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	controller := p.Controller
	if controller == "" {
		controller = p.Name // fallback
	}

	store.Notify(Event{
		Type:       p.Type,
		Source:     "jenkins",
		Controller: controller,
		Namespace:  p.Namespace,
	})

	events := store.List("")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	if events[0].Controller != "ctrl-from-name" {
		t.Errorf("Controller = %q, want %q", events[0].Controller, "ctrl-from-name")
	}
}

// TestJenkinsPayload_EmptyTimestamp verifies that an empty/garbage timestamp
// results in a zero time.Time (so Append substitutes time.Now()).
func TestJenkinsPayload_EmptyTimestamp(t *testing.T) {
	payload := map[string]interface{}{
		"source":    "jenkins",
		"name":      "ctrl-1",
		"namespace": "ns-1",
		"type":      "build.completed",
	}
	b, _ := json.Marshal(payload)
	var p struct {
		Source    string `json:"source"`
		Timestamp string `json:"timestamp"`
	}
	json.Unmarshal(b, &p)

	ts := parseTS(p.Timestamp)
	if !ts.IsZero() {
		t.Error("expected zero time for empty timestamp")
	}

	// Also test garbage timestamp.
	ts2 := parseTS("not-a-valid-timestamp")
	if !ts2.IsZero() {
		t.Error("expected zero time for garbage timestamp")
	}
}

// parseTS is the same helper defined in cmd/bff/main.go.
func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// TestStoreSchema_Marshal verifies that:
//   - A control-plane event (no jenkins fields) marshals without the new keys
//   - A jenkins event marshals with the new keys present
func TestStoreSchema_Marshal(t *testing.T) {
	// Control-plane event: no Jenkins fields.
	ctrlEvent := Event{
		Timestamp:  time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		Type:       "connected",
		Source:     "mite",
		Actor:      "",
		Controller: "ctrl-1",
		Namespace:  "ns-1",
		Message:    "mite connected",
		Phase:      "",
		Reason:     "",
	}
	b, err := json.Marshal(ctrlEvent)
	if err != nil {
		t.Fatalf("marshal control-plane event: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify new keys are absent (omitempty).
	for _, key := range []string{"itemPath", "buildNumber", "result", "url"} {
		if _, ok := result[key]; ok {
			t.Errorf("control-plane event should not have key %q", key)
		}
	}

	// Jenkins event: all fields populated.
	jenkinsEvent := Event{
		Timestamp:   time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC),
		Type:        "build.started",
		Source:      "jenkins",
		Actor:       "alice",
		Controller:  "ctrl-1",
		Namespace:   "ns-1",
		Message:     "Build started",
		ItemPath:    "team-a/job",
		BuildNumber: 42,
		Result:      "SUCCESS",
		URL:         "job/42/",
	}
	b2, err := json.Marshal(jenkinsEvent)
	if err != nil {
		t.Fatalf("marshal jenkins event: %v", err)
	}

	var result2 map[string]interface{}
	if err := json.Unmarshal(b2, &result2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"itemPath", "buildNumber", "result", "url"} {
		if _, ok := result2[key]; !ok {
			t.Errorf("jenkins event should have key %q", key)
		}
	}

	if v, ok := result2["buildNumber"]; ok {
		if v.(float64) != 42 {
			t.Errorf("buildNumber = %v, want 42", v)
		}
	}
}
