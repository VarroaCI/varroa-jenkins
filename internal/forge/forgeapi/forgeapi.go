// Package forgeapi holds the wire types of the operator forge verbs. It
// imports the standard library only, so the API tier can decode the replies
// without importing the forge client.
package forgeapi

import "time"

// Verbs of the operator forge subject. The subject is built by bus.OperatorForgeSubject.
const (
	VerbCheck       = "check"
	VerbRead        = "read"
	VerbPullRequest = "pullrequest"
	// VerbIssue opens an issue for a Team agent. The operator shard that owns
	// the Team answers it.
	VerbIssue = "issue"
)

// ProposalBranch is the branch that a pipeline proposal writes.
const ProposalBranch = "varroa/propose-pipeline"

// MaxRead is the largest file body that one read reply carries before the
// reply is cut.
const MaxRead = 262144

// RequestTimeout bounds one check or read request on the requester side.
const RequestTimeout = 30 * time.Second

// PullRequestTimeout bounds one pull-request request on the requester side. A
// pull request takes many forge calls.
const PullRequestTimeout = 90 * time.Second

// Reply codes. A reply with OK false sets exactly one of these.
const (
	CodeRepoURLInvalid       = "repo_url_invalid"
	CodeNoMatchingCredential = "no_matching_credential"
	CodeForgeUnsupported     = "forge_unsupported"
	CodeForgeAuthFailed      = "forge_auth_failed"
	CodeForgeWriteDenied     = "forge_write_denied"
	CodeForgeUnreachable     = "forge_unreachable"
	CodeJenkinsfileExists    = "jenkinsfile_exists"
	CodeProposalBranchExists = "proposal_branch_exists"
	// CodeProposalBaseMoved refuses a pull request whose base branch no
	// longer holds the commit that the check read.
	CodeProposalBaseMoved = "proposal_base_moved"
	// CodeRequestExpired refuses a pull request or an issue whose deadline leaves too little
	// work time, for example after a wait in the operator queue. No forge call was made.
	CodeRequestExpired = "request_expired"
	CodeInvalidRequest = "invalid_request"
	CodeNotFound       = "not_found"
	CodeInternal       = "internal"
)

// Status is the envelope of every forge reply.
type Status struct {
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// TeamAgentRef names the TeamAgent object that asks for an issue. The UID
// must match the stored object.
type TeamAgentRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// IssueRequest asks the operator to open an issue for a Team agent.
type IssueRequest struct {
	Team      string       `json:"team"`
	TeamAgent TeamAgentRef `json:"teamagent"`
	Title     string       `json:"title"`
	Body      string       `json:"body"`
	Labels    []string     `json:"labels,omitempty"`
	Actor     string       `json:"actor"`
	SessionID string       `json:"session_id"`
	// Deadline is the time when the requester stops waiting for the reply. The
	// operator refuses a request that has too little time left before it, and
	// makes no forge call.
	Deadline time.Time `json:"deadline"`
}

// IssueReply reports the issue that the forge opened.
type IssueReply struct {
	Status
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
}

// CheckRequest asks whether a repository can take a proposal.
type CheckRequest struct {
	Team string `json:"team"`
	Repo string `json:"repo"`
}

// CheckReply reports the result of a check.
type CheckReply struct {
	Status
	Forge        string `json:"forge,omitempty"`
	BaseBranch   string `json:"baseBranch,omitempty"`
	CredentialID string `json:"credentialId,omitempty"`
	// Revision is the commit of the base branch that the check read.
	Revision string `json:"revision,omitempty"`
}

// ReadRequest asks for a directory listing or a file of a repository.
type ReadRequest struct {
	Team     string `json:"team"`
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Revision string `json:"revision,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// ReadFile carries the bytes of a file. Encoding is "binary" when the file is
// not valid UTF-8, and Content is then empty.
type ReadFile struct {
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
	Encoding  string `json:"encoding,omitempty"`
}

// DirEntry is one child of a directory. Type is "file" or "dir".
type DirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ReadDir carries the children of a directory. It is never nil on a directory reply.
type ReadDir struct {
	Entries []DirEntry `json:"entries"`
}

// ReadReply answers a read request. A file reply sets the ReadFile fields and
// a directory reply sets the ReadDir fields.
type ReadReply struct {
	Status
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path"`
	*ReadFile
	*ReadDir
}

// File is one file of a pull-request commit.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// PullRequestRequest asks the operator to open a pull request. Its handler is
// added with the pullrequest verb.
type PullRequestRequest struct {
	Team          string `json:"team"`
	Repo          string `json:"repo"`
	Branch        string `json:"branch"`
	Title         string `json:"title"`
	Body          string `json:"body"`
	Files         []File `json:"files"`
	CommitMessage string `json:"commitMessage"`
	// BaseRevision, when set, is the base commit that the check read. The
	// pull request refuses when the base branch has moved past it.
	BaseRevision string `json:"baseRevision,omitempty"`
	// Deadline is the time when the requester stops waiting for the reply. The
	// operator refuses a request that has too little time left before it, and
	// makes no forge call.
	Deadline time.Time `json:"deadline"`
}

// PullRequestReply answers a pull-request request.
type PullRequestReply struct {
	Status
	URL    string `json:"url,omitempty"`
	Number int    `json:"number,omitempty"`
}

// CommitStatusRequest asks the operator to end the pending commit statuses
// of one build. The recorder sends it when it ends a run that Jenkins never
// completes. It has no reply: the operator logs the outcome.
type CommitStatusRequest struct {
	Team        string `json:"team"`
	Repo        string `json:"repo"`
	SHA         string `json:"sha"`
	JobPath     string `json:"jobPath"`
	BuildNumber int64  `json:"buildNumber"`
	Reason      string `json:"reason"`
}
