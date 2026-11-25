package main

import (
	"errors"
	"net/http"
	"testing"
)

// A request that no operator received prints the server text and keeps the
// operator_unavailable reason.
func TestErrFromResponse_KeepsTheOperatorUnavailableAnswer(t *testing.T) {
	const text = "no operator of cluster core received the request: the operator restarts or has no leader; try again later"
	err := errFromResponse([]byte(`{"error":"`+text+`","code":"operator_unavailable"}`), http.StatusServiceUnavailable)
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) || statusErr.code != http.StatusServiceUnavailable || statusErr.reason != "operator_unavailable" {
		t.Fatalf("err = %#v, want a 503 with the operator_unavailable reason", err)
	}
	if want := "error from server (503): " + text; err.Error() != want {
		t.Errorf("err = %q, want %q", err.Error(), want)
	}
}
