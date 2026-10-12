package api

import "encoding/json"

// changeRequestJSON is the changeRequest object of a run. A branch run
// has no such object. The id is always present when the object is.
type changeRequestJSON struct {
	ID           string `json:"id"`
	Title        string `json:"title,omitempty"`
	SourceBranch string `json:"sourceBranch,omitempty"`
	TargetBranch string `json:"targetBranch,omitempty"`
	URL          string `json:"url,omitempty"`
}

// changeRequestOf reads the change request of a run from a runs.payload
// column. It answers nil when the run has no change request or when the
// payload is malformed.
func changeRequestOf(payload json.RawMessage) *changeRequestJSON {
	if len(payload) == 0 {
		return nil
	}
	var doc struct {
		Started struct {
			Payload struct {
				ChangeRequest struct {
					ID           string `json:"id"`
					Title        string `json:"title"`
					SourceBranch string `json:"source_branch"`
					TargetBranch string `json:"target_branch"`
					URL          string `json:"url"`
				} `json:"change_request"`
			} `json:"payload"`
		} `json:"run.started"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil {
		return nil
	}
	cr := doc.Started.Payload.ChangeRequest
	if cr.ID == "" {
		return nil
	}
	return &changeRequestJSON{
		ID:           cr.ID,
		Title:        cr.Title,
		SourceBranch: cr.SourceBranch,
		TargetBranch: cr.TargetBranch,
		URL:          cr.URL,
	}
}
