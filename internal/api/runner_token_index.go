package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// RunnerTokenIndex maps a Runner's current door token to its name by the
// token's SHA-256 digest, never the raw token. It lets the agent door find
// a candidate Runner in one lookup instead of scanning every Runner CRD on
// every connection attempt (design D7). The BFF rebuilds it from its own
// Runner watch: an add or update event calls Put with the object's current
// name and status.token, and a delete event calls Remove.
//
// The index only narrows the search. The door still re-reads the candidate
// Runner CRD and re-verifies the raw token against its live status.token in
// constant time before trusting it, so a stale index entry can only widen
// the search, never grant access on its own.
type RunnerTokenIndex struct {
	mu  sync.RWMutex
	idx map[string]string // hex(sha256(token)) -> runner name
}

// NewRunnerTokenIndex returns an empty index.
func NewRunnerTokenIndex() *RunnerTokenIndex {
	return &RunnerTokenIndex{idx: make(map[string]string)}
}

// digestHex hex-encodes token's SHA-256 digest.
func digestHex(token string) string {
	d := sha256.Sum256([]byte(token))
	return hex.EncodeToString(d[:])
}

// Put indexes name under token's digest. Any digest already indexing name
// is dropped first, so a rotated token replaces the old entry rather than
// accumulating one, and the old token stops resolving. An empty token
// indexes nothing: a runner with no minted token (an ssh runner, or an
// inbound runner the operator has not reconciled yet) never resolves at
// the door.
func (t *RunnerTokenIndex) Put(name, token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for digest, n := range t.idx {
		if n == name {
			delete(t.idx, digest)
		}
	}
	if token == "" {
		return
	}
	t.idx[digestHex(token)] = name
}

// Remove drops every entry naming name.
func (t *RunnerTokenIndex) Remove(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for digest, n := range t.idx {
		if n == name {
			delete(t.idx, digest)
		}
	}
}

// Resolve returns the Runner name whose indexed digest matches token. An
// empty token, or one matching no indexed digest, answers "", false.
func (t *RunnerTokenIndex) Resolve(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	name, ok := t.idx[digestHex(token)]
	return name, ok
}
