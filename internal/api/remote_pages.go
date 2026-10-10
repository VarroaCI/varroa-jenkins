package api

import (
	"context"

	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// remoteListMaxPages bounds the pages one remote list reads.
const remoteListMaxPages = 128

// readRemotePages calls fetch with "" and then with each returned next key
// until next is "". It checks ctx before each page. A cancelled context, a
// page cap, or a cursor that does not advance returns a *BroodError with
// bus.CodeInternal. An error from fetch is returned unchanged.
func readRemotePages(ctx context.Context, fetch func(after string) (next string, err error)) error {
	after := ""
	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return &BroodError{Code: bus.CodeInternal, Msg: "list cancelled"}
		}
		if page == remoteListMaxPages {
			return &BroodError{Code: bus.CodeInternal, Msg: "list exceeds 128 pages"}
		}
		next, err := fetch(after)
		if err != nil {
			return err
		}
		if next == "" {
			return nil
		}
		if next <= after {
			return &BroodError{Code: bus.CodeInternal, Msg: "list cursor did not advance"}
		}
		after = next
	}
}
