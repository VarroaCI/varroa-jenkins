package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// RunConformance runs the same behavior suite against any Store driver.
// newStore builds one Store the whole suite shares. writeNative writes
// data at key the way a foreign writer would: a direct row insert with
// chunk_seq -1 for pgblob, or a direct bucket.NewWriter at the bare key
// for a gocloud.dev driver.
func RunConformance(
	t *testing.T,
	newStore func(t *testing.T) Store,
	writeNative func(t *testing.T, s Store, key string, data []byte, opts ObjectOptions),
) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)
	// Every key this suite writes starts with "conformance/". Removing
	// that prefix at the end leaves the store clean for the next run,
	// without depending on a caller that drops the whole schema.
	t.Cleanup(func() {
		_ = store.DeletePrefix(ctx, "conformance/")
	})

	// createdAt must fall inside a partition the migration created,
	// which spans the month before and the two months after "now" at
	// migration time. The current moment always lands inside that
	// window.
	createdAt := time.Now().UTC()
	logOpts := ObjectOptions{ObjectClass: "log", ContentType: "text/plain", CreatedAt: createdAt}
	artifactOpts := ObjectOptions{ObjectClass: "artifact", ContentType: "application/octet-stream", CreatedAt: createdAt}

	t.Run("PutThenPutAgainFails", func(t *testing.T) {
		key := "conformance/put"
		if err := store.Put(ctx, key, strings.NewReader("hello"), logOpts); err != nil {
			t.Fatalf("first put: %v", err)
		}
		if err := store.Put(ctx, key, strings.NewReader("hello"), logOpts); !errors.Is(err, ErrObjectExists) {
			t.Fatalf("second put: got %v, want ErrObjectExists", err)
		}
	})

	t.Run("PutRejectsExistingParts", func(t *testing.T) {
		key := "conformance/put-existing-parts"
		if err := store.Append(ctx, key, 0, []byte("chunk"), logOpts); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := store.Put(ctx, key, strings.NewReader("whole"), logOpts); !errors.Is(err, ErrObjectExists) {
			t.Fatalf("put over appended parts: got %v, want ErrObjectExists", err)
		}
	})

	t.Run("ReplaceOverwritesStoredObject", func(t *testing.T) {
		key := "conformance/replace-existing"

		if err := store.Put(ctx, key, strings.NewReader("first bytes"), logOpts); err != nil {
			t.Fatalf("put: %v", err)
		}
		if err := store.Replace(ctx, key, strings.NewReader("second"), logOpts); err != nil {
			t.Fatalf("replace: got %v, want nil", err)
		}
		r, err := store.NewRangeReader(ctx, key, 0, -1)
		if err != nil {
			t.Fatalf("NewRangeReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "second" {
			t.Fatalf("got %q, want %q", got, "second")
		}
	})

	t.Run("ReplaceWritesEmptyKey", func(t *testing.T) {
		key := "conformance/replace-empty"

		if err := store.Replace(ctx, key, strings.NewReader("only bytes"), logOpts); err != nil {
			t.Fatalf("replace: got %v, want nil", err)
		}
		r, err := store.NewRangeReader(ctx, key, 0, -1)
		if err != nil {
			t.Fatalf("NewRangeReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "only bytes" {
			t.Fatalf("got %q, want %q", got, "only bytes")
		}
	})

	t.Run("AppendIdenticalRetrySucceeds", func(t *testing.T) {
		key := "conformance/append-retry"
		data := []byte("same bytes")
		if err := store.Append(ctx, key, 0, data, logOpts); err != nil {
			t.Fatalf("first append: %v", err)
		}
		if err := store.Append(ctx, key, 0, data, logOpts); err != nil {
			t.Fatalf("identical retry: got %v, want nil", err)
		}
	})

	t.Run("AppendConflictingRetryFails", func(t *testing.T) {
		key := "conformance/append-conflict"
		if err := store.Append(ctx, key, 0, []byte("first"), logOpts); err != nil {
			t.Fatalf("first append: %v", err)
		}
		if err := store.Append(ctx, key, 0, []byte("second"), logOpts); !errors.Is(err, ErrChunkConflict) {
			t.Fatalf("conflicting retry: got %v, want ErrChunkConflict", err)
		}
	})

	readKey := func(t *testing.T, key string) string {
		t.Helper()
		r, err := store.NewRangeReader(ctx, key, 0, -1)
		if err != nil {
			t.Fatalf("NewRangeReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(got)
	}

	stampOf := func(p []byte, writer string) Stamp {
		return Stamp{Digest: Digest(p), Writer: writer}
	}

	t.Run("OverwriteReplacesAPartWithTheExpectedStamp", func(t *testing.T) {
		key := "conformance/overwrite-replace"
		if err := store.Append(ctx, key, 0, []byte("old "), logOpts); err != nil {
			t.Fatalf("append 0: %v", err)
		}
		if err := store.Append(ctx, key, 1, []byte("stale"), logOpts); err != nil {
			t.Fatalf("append 1: %v", err)
		}
		if err := store.Overwrite(ctx, key, 1, []byte("fresh"), stampOf([]byte("stale"), ""), logOpts); err != nil {
			t.Fatalf("overwrite 1: %v", err)
		}
		if got := readKey(t, key); got != "old fresh" {
			t.Fatalf("got %q, want %q", got, "old fresh")
		}
	})

	t.Run("OverwriteRefusesAStaleExpectedDigest", func(t *testing.T) {
		key := "conformance/overwrite-stale"
		if err := store.Append(ctx, key, 0, []byte("first"), logOpts); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := store.Overwrite(ctx, key, 0, []byte("second"), stampOf([]byte("first"), ""), logOpts); err != nil {
			t.Fatalf("first overwrite: %v", err)
		}
		err := store.Overwrite(ctx, key, 0, []byte("third"), stampOf([]byte("first"), ""), logOpts)
		if !errors.Is(err, ErrChunkConflict) {
			t.Fatalf("overwrite with a stale stamp: got %v, want ErrChunkConflict", err)
		}
		if got := readKey(t, key); got != "second" {
			t.Fatalf("got %q, want %q", got, "second")
		}
	})

	t.Run("OverwriteRefusesAnAbsentPart", func(t *testing.T) {
		key := "conformance/overwrite-absent"
		err := store.Overwrite(ctx, key, 0, []byte("first"), stampOf(nil, ""), logOpts)
		if !errors.Is(err, ErrChunkConflict) {
			t.Fatalf("overwrite of an absent part: got %v, want ErrChunkConflict", err)
		}
		if parts, err := store.Parts(ctx, key, 0); err != nil || len(parts) != 0 {
			t.Fatalf("parts = %v, %v, want none", parts, err)
		}
	})

	t.Run("OverwriteRefusesSameBytesOfAnotherWriter", func(t *testing.T) {
		key := "conformance/overwrite-other-writer"
		stored := logOpts
		stored.Writer = "ns/cell/inst2"
		if err := store.Append(ctx, key, 0, []byte{}, stored); err != nil {
			t.Fatalf("append: %v", err)
		}
		err := store.Overwrite(ctx, key, 0, []byte("late"), stampOf(nil, "ns/cell/inst1"), logOpts)
		if !errors.Is(err, ErrChunkConflict) {
			t.Fatalf("overwrite expecting another writer: got %v, want ErrChunkConflict", err)
		}
		if got := readKey(t, key); got != "" {
			t.Fatalf("got %q, want the empty part", got)
		}
	})

	t.Run("AppendAfterOverwriteKeepsWriteOnce", func(t *testing.T) {
		key := "conformance/overwrite-then-append"
		if err := store.Append(ctx, key, 0, []byte("stale"), logOpts); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := store.Overwrite(ctx, key, 0, []byte("fresh"), stampOf([]byte("stale"), ""), logOpts); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
		if err := store.Append(ctx, key, 0, []byte("fresh"), logOpts); err != nil {
			t.Fatalf("identical append after overwrite: got %v, want nil", err)
		}
		if err := store.Append(ctx, key, 0, []byte("stale"), logOpts); !errors.Is(err, ErrChunkConflict) {
			t.Fatalf("append of the replaced bytes: got %v, want ErrChunkConflict", err)
		}
	})

	t.Run("PartStampAnswersDigestAndWriter", func(t *testing.T) {
		key := "conformance/part-stamp"
		stamped := logOpts
		stamped.Writer = "ns/cell/inst1"
		if err := store.Append(ctx, key, 0, []byte("a"), stamped); err != nil {
			t.Fatalf("append stamped part: %v", err)
		}
		if err := store.Append(ctx, key, 1, []byte("b"), logOpts); err != nil {
			t.Fatalf("append unstamped part: %v", err)
		}
		if got, err := store.PartStamp(ctx, key, 0); err != nil || got != stampOf([]byte("a"), "ns/cell/inst1") {
			t.Fatalf("PartStamp(0) = %+v, %v, want the digest of a and ns/cell/inst1", got, err)
		}
		if got, err := store.PartStamp(ctx, key, 1); err != nil || got != stampOf([]byte("b"), "") {
			t.Fatalf("PartStamp(1) = %+v, %v, want the digest of b and an empty writer", got, err)
		}
		if _, err := store.PartStamp(ctx, key, 2); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("PartStamp(2): got %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("OverwriteReplacesTheWriter", func(t *testing.T) {
		key := "conformance/overwrite-writer"
		first := logOpts
		first.Writer = "ns/cell/inst1"
		if err := store.Append(ctx, key, 0, []byte("old"), first); err != nil {
			t.Fatalf("append: %v", err)
		}
		second := logOpts
		second.Writer = "ns/cell/inst2"
		if err := store.Overwrite(ctx, key, 0, []byte{}, stampOf([]byte("old"), "ns/cell/inst1"), second); err != nil {
			t.Fatalf("overwrite: %v", err)
		}
		if got, err := store.PartStamp(ctx, key, 0); err != nil || got != stampOf(nil, "ns/cell/inst2") {
			t.Fatalf("PartStamp = %+v, %v, want the empty part of ns/cell/inst2", got, err)
		}
		if err := store.Append(ctx, key, 0, []byte("late"), first); !errors.Is(err, ErrChunkConflict) {
			t.Fatalf("late append over the emptied part: got %v, want ErrChunkConflict", err)
		}
		if got, err := store.PartStamp(ctx, key, 0); err != nil || got.Writer != "ns/cell/inst2" {
			t.Fatalf("PartStamp after the refused append = %+v, %v, want ns/cell/inst2", got, err)
		}
	})

	t.Run("PartsListsPartsAboveAHoleWithSizes", func(t *testing.T) {
		key := "conformance/parts"
		for seq, data := range map[int64][]byte{0: []byte("a"), 1: []byte("bb"), 3: {}, 5: []byte("ccccc")} {
			if err := store.Append(ctx, key, seq, data, logOpts); err != nil {
				t.Fatalf("append %d: %v", seq, err)
			}
		}
		got, err := store.Parts(ctx, key, 1)
		if err != nil {
			t.Fatalf("Parts: %v", err)
		}
		if want := "[{1 2} {3 0} {5 5}]"; fmt.Sprint(got) != want {
			t.Fatalf("Parts from 1 = %v, want %s", got, want)
		}
		none, err := store.Parts(ctx, "conformance/parts-absent", 0)
		if err != nil {
			t.Fatalf("Parts of an absent key: %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("Parts of an absent key = %v, want none", none)
		}
	})

	t.Run("RangeReaderFull", func(t *testing.T) {
		key := "conformance/range-full"
		if err := store.Append(ctx, key, 0, []byte("hello "), logOpts); err != nil {
			t.Fatalf("append 0: %v", err)
		}
		if err := store.Append(ctx, key, 1, []byte("world"), logOpts); err != nil {
			t.Fatalf("append 1: %v", err)
		}
		r, err := store.NewRangeReader(ctx, key, 0, -1)
		if err != nil {
			t.Fatalf("NewRangeReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "hello world" {
			t.Fatalf("got %q, want %q", got, "hello world")
		}
	})

	t.Run("RangeReaderOffsetLength", func(t *testing.T) {
		key := "conformance/range-offset-length"
		if err := store.Append(ctx, key, 0, []byte("0123456789"), logOpts); err != nil {
			t.Fatalf("append: %v", err)
		}
		r, err := store.NewRangeReader(ctx, key, 2, 3)
		if err != nil {
			t.Fatalf("NewRangeReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "234" {
			t.Fatalf("got %q, want %q", got, "234")
		}
	})

	t.Run("RangeReaderGapShortRead", func(t *testing.T) {
		key := "conformance/range-gap"
		if err := store.Append(ctx, key, 0, []byte("aaa"), logOpts); err != nil {
			t.Fatalf("append 0: %v", err)
		}
		if err := store.Append(ctx, key, 1, []byte("bbb"), logOpts); err != nil {
			t.Fatalf("append 1: %v", err)
		}
		if err := store.Append(ctx, key, 3, []byte("ddd"), logOpts); err != nil {
			t.Fatalf("append 3: %v", err)
		}
		r, err := store.NewRangeReader(ctx, key, 0, -1)
		if err != nil {
			t.Fatalf("NewRangeReader: %v", err)
		}
		defer func() { _ = r.Close() }()
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read stopped at a gap should not error: %v", err)
		}
		if string(got) != "aaabbb" {
			t.Fatalf("got %q, want %q", got, "aaabbb")
		}
	})

	t.Run("RangeReaderInvalidRange", func(t *testing.T) {
		key := "conformance/range-invalid"
		if _, err := store.NewRangeReader(ctx, key, -1, -1); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("negative offset: got %v, want ErrInvalidRange", err)
		}
		if _, err := store.NewRangeReader(ctx, key, 0, -2); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("length below -1: got %v, want ErrInvalidRange", err)
		}
	})

	t.Run("RangeReaderMissing", func(t *testing.T) {
		key := "conformance/range-missing"
		if _, err := store.NewRangeReader(ctx, key, 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("missing object: got %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("NativeObjectLifecycle", func(t *testing.T) {
		key := "conformance/native-lifecycle"
		data := []byte("a native whole object written by a foreign writer")
		writeNative(t, store, key, data, artifactOpts)

		r, size, err := store.NewObjectReader(ctx, key, 0, -1)
		if err != nil {
			t.Fatalf("NewObjectReader whole: %v", err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("read whole: %v", err)
		}
		if size != int64(len(data)) {
			t.Fatalf("size: got %d, want %d", size, len(data))
		}
		if string(got) != string(data) {
			t.Fatalf("whole read mismatch: got %q, want %q", got, data)
		}

		r2, size2, err := store.NewObjectReader(ctx, key, 2, 4)
		if err != nil {
			t.Fatalf("NewObjectReader range: %v", err)
		}
		got2, err := io.ReadAll(r2)
		_ = r2.Close()
		if err != nil {
			t.Fatalf("read range: %v", err)
		}
		if size2 != int64(len(data)) {
			t.Fatalf("range size: got %d, want %d", size2, len(data))
		}
		if string(got2) != string(data[2:6]) {
			t.Fatalf("range read mismatch: got %q, want %q", got2, data[2:6])
		}

		if _, err := store.NewRangeReader(ctx, key, 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("NewRangeReader on a native-only key: got %v, want ErrObjectNotFound", err)
		}

		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("first delete: %v", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("second delete: got %v, want nil", err)
		}
		if _, _, err := store.NewObjectReader(ctx, key, 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("read after delete: got %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("ObjectReaderMissingWhenPartsExist", func(t *testing.T) {
		key := "conformance/object-only-parts"
		if err := store.Append(ctx, key, 0, []byte("chunk"), logOpts); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, _, err := store.NewObjectReader(ctx, key, 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("NewObjectReader on a chunked-only key: got %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("ObjectReaderMissing", func(t *testing.T) {
		key := "conformance/object-missing"
		if _, _, err := store.NewObjectReader(ctx, key, 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("missing object: got %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("DeleteIdempotentOnParts", func(t *testing.T) {
		key := "conformance/delete-parts"
		if err := store.Append(ctx, key, 0, []byte("x"), logOpts); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("first delete: %v", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("second delete: got %v, want nil", err)
		}
		if _, err := store.NewRangeReader(ctx, key, 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("read after delete: got %v, want ErrObjectNotFound", err)
		}
	})

	t.Run("DeletePrefixRemovesOnlyMatches", func(t *testing.T) {
		if err := store.Append(ctx, "conformance/prefix/a", 0, []byte("a"), logOpts); err != nil {
			t.Fatalf("append a: %v", err)
		}
		if err := store.Append(ctx, "conformance/prefix/b", 0, []byte("b"), logOpts); err != nil {
			t.Fatalf("append b: %v", err)
		}
		if err := store.Append(ctx, "conformance/prefixed-other", 0, []byte("c"), logOpts); err != nil {
			t.Fatalf("append other: %v", err)
		}

		if err := store.DeletePrefix(ctx, "conformance/prefix/"); err != nil {
			t.Fatalf("DeletePrefix: %v", err)
		}

		if _, err := store.NewRangeReader(ctx, "conformance/prefix/a", 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("prefix/a survived DeletePrefix: got %v, want ErrObjectNotFound", err)
		}
		if _, err := store.NewRangeReader(ctx, "conformance/prefix/b", 0, -1); !errors.Is(err, ErrObjectNotFound) {
			t.Fatalf("prefix/b survived DeletePrefix: got %v, want ErrObjectNotFound", err)
		}
		if _, err := store.NewRangeReader(ctx, "conformance/prefixed-other", 0, -1); err != nil {
			t.Fatalf("an unrelated key was deleted: %v", err)
		}
	})

	t.Run("InvalidOptions", func(t *testing.T) {
		key := "conformance/invalid-options"
		emptyClass := ObjectOptions{ContentType: "text/plain", CreatedAt: createdAt}
		if err := store.Append(ctx, key, 0, []byte("x"), emptyClass); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("empty object class: got %v, want ErrInvalidOptions", err)
		}
		zeroCreatedAt := ObjectOptions{ObjectClass: "log", ContentType: "text/plain"}
		if err := store.Put(ctx, key, strings.NewReader("x"), zeroCreatedAt); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("zero created_at: got %v, want ErrInvalidOptions", err)
		}
	})

	t.Run("SignedURL", func(t *testing.T) {
		key := "conformance/signed-url"
		if err := store.Put(ctx, key, strings.NewReader("signed"), artifactOpts); err != nil {
			t.Fatalf("put: %v", err)
		}
		url, err := store.SignedURL(ctx, key, 5*time.Minute)
		if errors.Is(err, ErrUnsupported) {
			// pgblob's own answer for a tier that cannot sign a URL at
			// all.
			return
		}
		if err != nil {
			t.Fatalf("signed url: %v", err)
		}
		if url == "" {
			t.Fatal("signed url: got an empty string with no error")
		}
	})
}
