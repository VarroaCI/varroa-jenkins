package transport

import "time"

// ClassifiedInventory is the stored classification result persisted by the
// operator and served by the BFF. Class and DeclaredBy are string labels (R19).
type ClassifiedInventory struct {
	Envelope   ClassifiedEnvelope
	Plugins    []ClassifiedPlugin
	Advisories []Advisory
}

// ClassifiedEnvelope mirrors the frozen §7 status block's summary fields exactly.
type ClassifiedEnvelope struct {
	Hash                 string
	CollectedAt          time.Time
	ObservedAt           time.Time
	Source               string
	Stale                bool
	Degraded             bool
	BootstrapApproximate bool
	OptionalEdgesDropped bool
	Truncated            bool
	Total                int
	Counts               map[string]int
	DriftTruncated       bool
}

// ClassifiedPlugin is one classified installed plugin.
type ClassifiedPlugin struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Class        string   `json:"class"` // string label, never ordinal (R19)
	DeclaredBy   string   `json:"declaredBy,omitempty"`
	Contributors []string `json:"contributors,omitempty"`
	ImpliedBy    []string `json:"impliedBy,omitempty"`
	// VersionVerdict is ahead/behind/missing for class-2 only.
	VersionVerdict string `json:"versionVerdict,omitempty"`
	Enabled        string `json:"enabled,omitempty"`
	Detached       string `json:"detached,omitempty"`
	Bundled        string `json:"bundled,omitempty"`
}

// Advisory is a non-classification finding.
type Advisory struct {
	Code       string `json:"code"`
	Plugin     string `json:"plugin"`
	Dependency string `json:"dependency"`
	Min        string `json:"min"`
	Version    string `json:"version"`
}
