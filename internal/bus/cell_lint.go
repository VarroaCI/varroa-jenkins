package bus

// CellLintRequest is the payload for cells.lint. The member operator lints
// Jenkinsfile on the named cell of its own cluster.
type CellLintRequest struct {
	Namespace   string `json:"namespace"`
	Cell        string `json:"cell"`
	Jenkinsfile string `json:"jenkinsfile"`
}

// CellLintFinding is one finding of a Jenkinsfile lint. Its JSON shape
// matches the lint finding that the BFF answers to its callers.
type CellLintFinding struct {
	Source  string `json:"source"`
	Rule    string `json:"rule,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
}

// CellLintResponse is the reply for cells.lint. Errors has no omitempty, so a
// valid lint sends an empty array. Phase is set when the cell is not ready to
// lint, and Code is set with it. LinterError is set when the cell linter
// could not answer. Code and Error are set on a failed read.
type CellLintResponse struct {
	Errors      []CellLintFinding `json:"errors"`
	Phase       string            `json:"phase,omitempty"`
	LinterError string            `json:"linterError,omitempty"`
	Code        string            `json:"code,omitempty"`
	Error       string            `json:"error,omitempty"`
}
