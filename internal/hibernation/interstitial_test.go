package hibernation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteInterstitial(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		redirect  bool
		wantRetry string
	}{
		{name: "BFF", status: http.StatusOK, redirect: false},
		{name: "operator", status: http.StatusServiceUnavailable, redirect: true, wantRetry: "5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteInterstitial(rec, InterstitialParams{
				StatusPath:                "/.varroa/wake/status?x=1",
				TargetURL:                 "/job/example/",
				HTTPStatus:                tt.status,
				RedirectOnNonWakeResponse: tt.redirect,
			})
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetry {
				t.Fatalf("Retry-After = %q, want %q", got, tt.wantRetry)
			}
			body := rec.Body.String()
			for _, want := range []string{`var statusPath = "/.varroa/wake/status?x=1";`, `var targetURL = "/job/example/";`, "if (r.status >= 500) return;", "d.varroaWake === true", "d.phase === 'Connected'"} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
			wantFlag := "var redirectOnNonWake = false;"
			if tt.redirect {
				wantFlag = "var redirectOnNonWake = true;"
			}
			if !strings.Contains(body, wantFlag) {
				t.Errorf("body missing %q", wantFlag)
			}
		})
	}
}
