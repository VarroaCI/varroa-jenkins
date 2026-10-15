package telemetry

import (
	"os"
	"testing"
)

// TestServesPrometheusMetrics asserts every known VARROA_SERVICE_NAME
// value that ships a /metrics endpoint, including varroa-recorder,
// reports true, and an unknown or unset name reports false.
func TestServesPrometheusMetrics(t *testing.T) {
	original, hadOriginal := os.LookupEnv("VARROA_SERVICE_NAME")
	t.Cleanup(func() {
		if hadOriginal {
			_ = os.Setenv("VARROA_SERVICE_NAME", original)
		} else {
			_ = os.Unsetenv("VARROA_SERVICE_NAME")
		}
	})

	tests := []struct {
		name    string
		service string
		want    bool
	}{
		{name: "bff", service: "varroa-bff", want: true},
		{name: "operator", service: "varroa-operator", want: true},
		{name: "gateway", service: "varroa-gateway", want: true},
		{name: "updatecenter", service: "varroa-updatecenter", want: true},
		{name: "recorder", service: "varroa-recorder", want: true},
		{name: "mite is not a Prometheus service", service: "varroa-mite", want: false},
		{name: "unset", service: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.service == "" {
				_ = os.Unsetenv("VARROA_SERVICE_NAME")
			} else {
				_ = os.Setenv("VARROA_SERVICE_NAME", tc.service)
			}
			if got := ServesPrometheusMetrics(); got != tc.want {
				t.Errorf("ServesPrometheusMetrics() with VARROA_SERVICE_NAME=%q = %v, want %v", tc.service, got, tc.want)
			}
		})
	}
}
