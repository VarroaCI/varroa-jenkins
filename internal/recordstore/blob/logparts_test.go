package blob

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// fixedPartLister answers a fixed part list, filtered by from, and a
// fixed writer for each sequence.
type fixedPartLister struct {
	parts   []Part
	writers map[int64]string
	err     error
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

func (f fixedPartLister) PartWriter(_ context.Context, _ string, seq int64) (string, error) {
	for _, p := range f.parts {
		if p.Seq == seq {
			return f.writers[seq], nil
		}
	}
	return "", ErrObjectNotFound
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
			got, _, err := UnindexedLogParts(context.Background(), fixedPartLister{parts: tc.parts}, "r1", 2, "")
			if err != nil {
				t.Fatalf("UnindexedLogParts: %v", err)
			}
			if fmt.Sprint(got) != tc.want {
				t.Fatalf("parts = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestUnindexedLogPartsStopsAtAWriterChange pins the writer rule: a part
// of another writer than the highest index entry's is never recovered,
// and neither is any part above it.
func TestUnindexedLogPartsStopsAtAWriterChange(t *testing.T) {
	lister := fixedPartLister{
		parts:   []Part{{2, 3}, {3, 3}, {4, 3}},
		writers: map[int64]string{2: "ns/b/i2", 3: "ns/a/i1", 4: "ns/b/i2"},
	}
	got, writer, err := UnindexedLogParts(context.Background(), lister, "r1", 2, "ns/b/i2")
	if err != nil {
		t.Fatalf("UnindexedLogParts: %v", err)
	}
	if fmt.Sprint(got) != "[{2 3}]" || writer != "ns/b/i2" {
		t.Fatalf("parts = %v, writer = %q, want [{2 3}] and ns/b/i2", got, writer)
	}

	got, _, err = UnindexedLogParts(context.Background(), lister, "r1", 2, "ns/a/i1")
	if err != nil {
		t.Fatalf("UnindexedLogParts: %v", err)
	}
	if fmt.Sprint(got) != "[]" {
		t.Fatalf("parts = %v, want none for a part of another writer at from", got)
	}
}

// TestUnindexedLogPartsBaselineIsTheFirstPartWithNoIndexWriter pins the
// baseline when no index entry names a writer: the part at from sets it.
func TestUnindexedLogPartsBaselineIsTheFirstPartWithNoIndexWriter(t *testing.T) {
	lister := fixedPartLister{
		parts:   []Part{{0, 3}, {1, 3}, {2, 3}},
		writers: map[int64]string{0: "ns/a/i1", 1: "ns/a/i1", 2: "ns/b/i2"},
	}
	got, writer, err := UnindexedLogParts(context.Background(), lister, "r1", 0, "")
	if err != nil {
		t.Fatalf("UnindexedLogParts: %v", err)
	}
	if fmt.Sprint(got) != "[{0 3} {1 3}]" || writer != "ns/a/i1" {
		t.Fatalf("parts = %v, writer = %q, want [{0 3} {1 3}] and ns/a/i1", got, writer)
	}
}

func TestUnindexedLogPartsListFailureIsAnError(t *testing.T) {
	if _, _, err := UnindexedLogParts(context.Background(), fixedPartLister{err: errors.New("down")}, "r1", 0, ""); err == nil {
		t.Fatal("want an error from a failing part listing")
	}
}
