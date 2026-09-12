package pipelinetemplate

import (
	"errors"
	"strings"
	"testing"
)

const validMarkerYAML = `
apiVersion: varroa.dev/v1
kind: Pipeline
template: go-service
templateVersion: "2.1"
`

// TestParseMarkerReader_ValidMarkerParses proves ParseMarkerReader parses
// a marker within the size limit the same way ParseMarker does.
func TestParseMarkerReader_ValidMarkerParses(t *testing.T) {
	m, err := ParseMarkerReader(strings.NewReader(validMarkerYAML))
	if err != nil {
		t.Fatalf("ParseMarkerReader: %v", err)
	}
	if m.Template != "go-service" {
		t.Errorf("template = %q, want go-service", m.Template)
	}
}

// unboundedReader never returns EOF and counts every byte it has ever
// served, so a test can prove a caller stopped reading it at a bound
// rather than draining it.
type unboundedReader struct {
	served int
}

func (r *unboundedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	r.served += len(p)
	return len(p), nil
}

// TestParseMarkerReader_OversizedSourceNeverFullyRead proves 🟡2 of
// review round 1: ParseMarkerReader rejects a marker over MaxMarkerSize
// without reading the whole source. An unbounded reader that never
// returns EOF would hang or exhaust memory if fully drained; this proves
// ParseMarkerReader stops at maxMarkerRead bytes instead.
func TestParseMarkerReader_OversizedSourceNeverFullyRead(t *testing.T) {
	r := &unboundedReader{}
	_, err := ParseMarkerReader(r)
	if err == nil {
		t.Fatal("expected an error for a source larger than MaxMarkerSize")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected an \"exceeds\" error, got: %v", err)
	}
	if r.served > maxMarkerRead {
		t.Errorf("served %d bytes, want at most %d: the reader must stop at the bound, not drain the source", r.served, maxMarkerRead)
	}
}

// TestParseMarkerReader_ReadError proves a read failure from the source
// surfaces as an error rather than a partial parse.
func TestParseMarkerReader_ReadError(t *testing.T) {
	_, err := ParseMarkerReader(errorReader{})
	if err == nil {
		t.Fatal("expected an error when the source fails to read")
	}
}

// errorReader fails every Read call.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errSimulatedRead
}

var errSimulatedRead = errors.New("simulated read failure")
