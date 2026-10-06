package recordstore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// LogIndexReader is the blob methods StoredLogNextSeq needs. The
// recorder passes its blob.Store value.
type LogIndexReader interface {
	NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
	blob.LogPartReader
}

// storedLogIndexEntry holds the fields of one gateway log index line
// that the next sequence depends on. The gateway owns the full shape.
type storedLogIndexEntry struct {
	Seq    int64  `json:"seq"`
	Writer string `json:"writer,omitempty"`
}

// StoredLogNextSeq answers the next log sequence that the gateway
// accepts for runID: the highest stored sequence with no hole below it,
// plus 1. It answers 0 when the run has no stored log.
//
// The rule must equal the gateway's own restart recovery. The gateway
// writes one recovered index entry for each part that
// blob.UnindexedLogParts names above the highest index entry, before it
// appends anything, so each such part is one more sequence. A lower
// answer names a sequence the gateway drops, and a higher answer leaves
// a hole that the gateway never fills.
func StoredLogNextSeq(ctx context.Context, store LogIndexReader, runID string) (int64, error) {
	entries, err := readStoredLogIndex(ctx, store, blob.LogIndexKey(runID))
	if err != nil {
		return 0, err
	}

	maxSeq := int64(-1)
	for seq := range entries {
		if seq > maxSeq {
			maxSeq = seq
		}
	}
	writer := ""
	for seq := maxSeq; seq >= 0 && writer == ""; seq-- {
		writer = entries[seq].Writer
	}
	unindexed, _, err := blob.UnindexedLogParts(ctx, store, runID, maxSeq+1, writer)
	if err != nil {
		return 0, fmt.Errorf("list log parts: %w", err)
	}
	for _, part := range unindexed {
		entries[part.Seq] = storedLogIndexEntry{Seq: part.Seq}
	}

	next := int64(0)
	for {
		if _, ok := entries[next]; !ok {
			break
		}
		next++
	}
	return next, nil
}

// readStoredLogIndex parses every ndjson line of the index at key, keyed
// by sequence. A missing index answers an empty map.
func readStoredLogIndex(ctx context.Context, store LogIndexReader, key string) (map[int64]storedLogIndexEntry, error) {
	entries := make(map[int64]storedLogIndexEntry)
	r, err := store.NewRangeReader(ctx, key, 0, -1)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			return entries, nil
		}
		return nil, fmt.Errorf("read log index: %w", err)
	}
	defer func() { _ = r.Close() }()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e storedLogIndexEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("parse log index line: %w", err)
		}
		entries[e.Seq] = e
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan log index: %w", err)
	}
	return entries, nil
}
