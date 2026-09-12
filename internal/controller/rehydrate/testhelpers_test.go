package rehydrate

import (
	"encoding/xml"
	"testing"
)

// mustUnmarshal decodes raw into v, failing t on error. Tests use it
// to assert on a rendered document's element structure instead of on
// its exact byte layout.
func mustUnmarshal(t *testing.T, raw []byte, v interface{}) {
	t.Helper()
	if err := xml.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal rendered document: %v\n%s", err, raw)
	}
}
