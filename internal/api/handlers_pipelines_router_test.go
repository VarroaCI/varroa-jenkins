package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestJobPathEncoding pins design D2: {job} is one path segment holding
// the percent-encoded job path. http.StripPrefix trims RawPath as well
// as Path, so the escaped form survives the prefix strip, and
// r.PathValue("job") returns the decoded value with its slashes intact.
func TestJobPathEncoding(t *testing.T) {
	mux := http.NewServeMux()
	var got string
	mux.HandleFunc("GET /pipelines/{team}/jobs/{job}/runs", func(w http.ResponseWriter, r *http.Request) {
		got = r.PathValue("job")
		w.WriteHeader(http.StatusOK)
	})
	handler := http.StripPrefix("/api/v1", mux)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/pipelines/payments/jobs/payments%2Fcheckout-api%2Fmain/runs")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if want := "payments/checkout-api/main"; got != want {
		t.Fatalf("job = %q, want %q", got, want)
	}
}
