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

// LogIndexReader is the one blob method StoredLogNextSeq needs. The
// recorder passes its blob.Store value.
type LogIndexReader interface {
	NewRangeReader(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)
}

// storedLogIndexEntry holds the fields of one gateway log index line
// that the next sequence depends on. The gateway owns the full shape.
type storedLogIndexEntry struct {
	Seq    int64 `json:"seq"`
	Offset int64 `json:"offset"`
	Len    int64 `json:"len"`
}

// StoredLogNextSeq answers the next log sequence that the gateway
// accepts for runID: the highest stored sequence with no hole below it,
// plus 1. It answers 0 when the run has no stored log.
//
// The rule must equal the gateway's own restart recovery. Log bytes past
// the end of the index entry with the highest sequence are one more
// sequence, because the gateway writes a recovered index entry for them
// before it appends anything. A lower answer names a sequence the gateway drops, and a
// higher answer leaves a hole that the gateway never fills.
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
	var covered int64
	if maxSeq >= 0 {
		covered = entries[maxSeq].Offset + entries[maxSeq].Len
	}
	trailing, err := hasLogBytesAt(ctx, store, blob.LogKey(runID), covered)
	if err != nil {
		return 0, err
	}
	// The gateway recovers the trailing bytes as one entry at maxSeq+1.
	if trailing {
		entries[maxSeq+1] = storedLogIndexEntry{Seq: maxSeq + 1}
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

// hasLogBytesAt reports whether the readable log at key holds a byte at
// offset.
func hasLogBytesAt(ctx context.Context, store LogIndexReader, key string, offset int64) (bool, error) {
	r, err := store.NewRangeReader(ctx, key, offset, 1)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("read log: %w", err)
	}
	defer func() { _ = r.Close() }()
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return false, fmt.Errorf("read log: %w", err)
	}
	return n > 0, nil
}
