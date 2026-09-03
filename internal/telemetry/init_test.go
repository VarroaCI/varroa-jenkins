package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzHandlerWithBus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		connected bool
		wantBus   string
	}{
		{name: "connected", connected: true, wantBus: "connected"},
		{name: "disconnected", connected: false, wantBus: "disconnected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			HealthzHandlerWithBus(func() bool { return tc.connected }).
				ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

			// A degraded bus never fails the probe; only the reported field changes.
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if body["status"] != "ok" {
				t.Errorf("status field = %q, want \"ok\"", body["status"])
			}
			if body["bus"] != tc.wantBus {
				t.Errorf("bus field = %q, want %q", body["bus"], tc.wantBus)
			}
		})
	}
}
