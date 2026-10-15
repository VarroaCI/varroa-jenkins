package activity

import (
	"strings"
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
