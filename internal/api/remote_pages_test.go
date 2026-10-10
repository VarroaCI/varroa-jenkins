package api

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func TestReadRemotePages_WalksUntilEmptyNext(t *testing.T) {
	next := map[string]string{"": "a", "a": "b", "b": ""}
	var seen []string
	err := readRemotePages(context.Background(), func(after string) (string, error) {
		seen = append(seen, after)
		return next[after], nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(seen) != 3 || seen[0] != "" || seen[1] != "a" || seen[2] != "b" {
		t.Fatalf("pages read after = %v, want [\"\" a b]", seen)
	}
}

func TestReadRemotePages_CursorMustAdvance(t *testing.T) {
	err := readRemotePages(context.Background(), func(after string) (string, error) {
		return "a", nil
	})
	// The first page moves the cursor from "" to "a". The second page
	// returns "a" again, which does not advance.
	var be *BroodError
	if !errors.As(err, &be) || be.Code != bus.CodeInternal || be.Msg != "list cursor did not advance" {
		t.Fatalf("err = %v, want cursor did not advance", err)
	}
}

func TestReadRemotePages_PageCap(t *testing.T) {
	calls := 0
	err := readRemotePages(context.Background(), func(after string) (string, error) {
		calls++
		// Each page moves the cursor forward, so only the cap stops the walk.
		return after + "x", nil
	})
	var be *BroodError
	if !errors.As(err, &be) || be.Msg != "list exceeds 128 pages" {
		t.Fatalf("err = %v, want page cap", err)
	}
	if calls != remoteListMaxPages {
		t.Fatalf("pages read = %d, want %d", calls, remoteListMaxPages)
	}
}

func TestReadRemotePages_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := readRemotePages(ctx, func(after string) (string, error) {
		calls++
		return "", nil
	})
	var be *BroodError
	if !errors.As(err, &be) || be.Msg != "list cancelled" {
		t.Fatalf("err = %v, want list cancelled", err)
	}
	if calls != 0 {
		t.Fatalf("fetch ran %d times after cancel, want 0", calls)
	}
}

func TestReadRemotePages_FetchErrorUnchanged(t *testing.T) {
	sentinel := errors.New("transport down")
	err := readRemotePages(context.Background(), func(after string) (string, error) {
		return "", sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
}
