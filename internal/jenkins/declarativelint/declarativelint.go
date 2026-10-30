// Package declarativelint calls the Jenkins declarative linter on one cell.
// The BFF and the member operator share it. It holds no state and reads no
// cluster object, so both sides can import it.
package declarativelint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxReply bounds the linter reply that Validate reads.
const MaxReply = 1 << 20

// HTTPClient calls the cell linter. The 20 second timeout must stay below
// the BFF relay timeout for a member lint.
var HTTPClient = &http.Client{Timeout: 20 * time.Second}

// Finding is one finding of a Jenkinsfile lint.
type Finding struct {
	Source  string `json:"source"`
	Rule    string `json:"rule,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
}

// linterLocation extracts the line and column that the declarative linter
// writes into its message text.
var linterLocation = regexp.MustCompile(`@ line (\d+), column (\d+)\.`)

// UnreachableError is a transport failure of the linter call. Error stays
// generic, because a caller may show it to a user. Unwrap returns the
// transport cause for the log.
type UnreachableError struct{ Err error }

// Error returns the generic message.
func (e *UnreachableError) Error() string { return "the cell linter is unreachable" }

// Unwrap returns the transport cause.
func (e *UnreachableError) Unwrap() error { return e.Err }

// Validate posts text to the declarative linter at baseURL with a Bearer
// token and parses the reply. The client message stays generic, so the
// transport detail never reaches a caller that shows it to a user.
func Validate(ctx context.Context, client *http.Client, baseURL, token, text string) ([]Finding, error) {
	form := url.Values{"jenkinsfile": {text}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/pipeline-model-converter/validateJenkinsfile", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build linter request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, &UnreachableError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the cell linter answered status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxReply))
	if err != nil {
		return nil, fmt.Errorf("read the cell linter reply: %w", err)
	}
	return ParseReply(raw)
}

// linterReply is the declarative linter's JSON envelope.
type linterReply struct {
	Status string `json:"status"`
	Data   struct {
		Result string `json:"result"`
		Errors []struct {
			Error json.RawMessage `json:"error"`
		} `json:"errors"`
	} `json:"data"`
}

// ParseReply turns the linter's JSON envelope into findings. An unknown
// envelope is an error, so a changed linter cannot pass as valid.
func ParseReply(body []byte) ([]Finding, error) {
	var reply linterReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, fmt.Errorf("the cell linter reply is not JSON")
	}
	if reply.Status != "ok" {
		return nil, fmt.Errorf("the cell linter reported status %q", reply.Status)
	}
	out := []Finding{}
	switch reply.Data.Result {
	case "success":
		return out, nil
	case "failure":
	default:
		return nil, fmt.Errorf("the cell linter reported result %q", reply.Data.Result)
	}
	for _, e := range reply.Data.Errors {
		for _, msg := range linterMessages(e.Error) {
			f := Finding{Source: "jenkins", Message: strings.TrimSpace(msg)}
			if m := linterLocation.FindStringSubmatch(msg); m != nil {
				f.Line, _ = strconv.Atoi(m[1])
				f.Column, _ = strconv.Atoi(m[2])
			}
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the cell linter reported failure without an error message")
	}
	return out, nil
}

// linterMessages decodes one error field, which is a string or a list of
// strings.
func linterMessages(raw json.RawMessage) []string {
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}
