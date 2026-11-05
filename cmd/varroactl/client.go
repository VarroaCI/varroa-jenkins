package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// apiClient constructs a pkg/client from the resolved context, injecting
// Authorization: Bearer and User-Agent: varroactl/<version>.
func apiClient(cmd *cobra.Command) (*client.ClientWithResponses, error) {
	rc, err := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return nil, err
	}

	baseURL := strings.TrimRight(rc.server, "/")

	c, err := client.New(baseURL, rc.apiKey, client.WithUserAgent("varroactl/"+version))
	if err != nil {
		return nil, fmt.Errorf("failed to create API client: %w", err)
	}
	return c, nil
}

// errFromResponse extracts an error message from a response body.
// Works for both consumed (WithResponse Body field) and unconsumed responses.
//
// A 409 from a controller write carries a `conflicts` array alongside `error`,
// naming each field a server-side apply refused to change and the field manager
// that owns it. The bare `error` is just "field conflict", which says a write
// was refused but neither what it collided with nor how to proceed — so the
// conflicts are rendered too, with the flag that resolves them.
func errFromResponse(body []byte, statusCode int) error {
	msg := string(body)
	if len(msg) > 512 {
		msg = msg[:512]
	}
	var env struct {
		Error     string `json:"error"`
		Conflicts []struct {
			Field   string `json:"field"`
			Manager string `json:"manager"`
		} `json:"conflicts"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.Error != "" {
			msg = env.Error
		}
		if len(env.Conflicts) > 0 {
			owned := make([]string, 0, len(env.Conflicts))
			for _, c := range env.Conflicts {
				owned = append(owned, fmt.Sprintf("%s (owned by %s)", c.Field, c.Manager))
			}
			msg += ": " + strings.Join(owned, ", ") +
				" — retry with --force to take ownership"
		}
	}
	return &httpStatusError{code: statusCode, msg: msg}
}

// httpStatusError is a non-2xx answer from the server. Its text is the same
// as the message errFromResponse always printed; callers test code to act on
// one status, such as a 409 on an edit.
type httpStatusError struct {
	code int
	msg  string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("error from server (%d): %s", e.code, e.msg)
}

// apiErrorf wraps a *client.APIError into a standard error message.
// Used for raw (non-WithResponse) HTTP responses where body is still available.
func apiErrorf(apiErr *client.APIError) error {
	if apiErr == nil {
		return nil
	}
	return errFromResponse(apiErr.Body, apiErr.StatusCode)
}
