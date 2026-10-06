package blob

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fixedPartLister answers a fixed part list, filtered by from.
type fixedPartLister struct {
	parts []Part
	err   error
}

func (f fixedPartLister) Parts(_ context.Context, _ string, from int64) ([]Part, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []Part
	for _, p := range f.parts {
		if p.Seq >= from {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestUnindexedLogParts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []Part
		want  string
	}{
		{name: "no part", want: "[]"},
		{name: "every readable part", parts: []Part{{2, 3}, {3, 8}}, want: "[{2 3} {3 8}]"},
		{name: "stops at a missing part", parts: []Part{{2, 3}, {4, 3}}, want: "[{2 3}]"},
		{name: "stops at an empty part", parts: []Part{{2, 3}, {3, 0}, {4, 3}}, want: "[{2 3}]"},
		{name: "a hole at from", parts: []Part{{3, 3}}, want: "[]"},
		{name: "an empty part at from", parts: []Part{{2, 0}, {3, 3}}, want: "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UnindexedLogParts(context.Background(), fixedPartLister{parts: tc.parts}, "r1", 2)
			if err != nil {
				t.Fatalf("UnindexedLogParts: %v", err)
			}
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("parts = %v, want %s", got, tc.want)
			}
		})
	}
}

func TestUnindexedLogPartsListFailureIsAnError(t *testing.T) {
	if _, err := UnindexedLogParts(context.Background(), fixedPartLister{err: errors.New("down")}, "r1", 0); err == nil {
		t.Fatal("want an error from a failing part listing")
	}
}
