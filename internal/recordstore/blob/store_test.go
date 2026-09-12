package blob

import (
	"io"
	"math"
	"testing"
)

// TestSliceReader_LengthNearMaxInt64DoesNotOverflow proves a length as
// large as math.MaxInt64 — what an unbounded Range header end
// ("bytes=1-9223372036854775807") produces — reads to the end of all
// instead of overflowing offset+length into a negative slice bound.
func TestSliceReader_LengthNearMaxInt64DoesNotOverflow(t *testing.T) {
	all := []byte("hello world")
	rc := sliceReader(all, 1, math.MaxInt64)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ello world" {
		t.Fatalf("got %q, want %q", got, "ello world")
	}
}

// TestSliceReader_ExactLengthAndOffsetPastEnd cover the ordinary bounds
// sliceReader already handled correctly, so the overflow guard above
// does not regress them.
func TestSliceReader_ExactLengthAndOffsetPastEnd(t *testing.T) {
	all := []byte("hello world")

	cases := []struct {
		name           string
		offset, length int64
		want           string
	}{
		{"exact length", 0, 5, "hello"},
		{"length -1 reads to end", 6, -1, "world"},
		{"length past end clamps to end", 6, 100, "world"},
		{"offset past end yields empty", 100, -1, ""},
		{"zero length yields empty", 0, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc := sliceReader(all, tc.offset, tc.length)
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
