package recordstore

import (
	"encoding/json"

	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

// RunScriptObjectKey answers the blob key of the build.xml the run retains,
// and false when the run's record names none.
//
// It lives here because runStateFile is unexported. One implementation then
// serves the read path and the cleanup, and the object the read wants is
// always the object the cleanup keeps.
func RunScriptObjectKey(run Run) (string, bool) {
	key, _, ok := RunScriptObject(run)
	return key, ok
}

// RunScriptObject answers the blob key and the recorded inflated size of
// the build.xml the run retains, and false when the run's record names
// none. A reader tests the size before it opens the object, so a document
// over its bound never reaches a parser as a truncated read.
func RunScriptObject(run Run) (key string, size int64, ok bool) {
	entry, ok := retainedScriptEntry(runStateFilesOfPayload(run.Payload))
	if !ok {
		return "", 0, false
	}
	return blob.RunStateKey(run.RunID, entry.Epoch, entry.StateGen, entry.RelPath), entry.Bytes, true
}

// runStateFilesOfPayload decodes runs.payload -> 'mesh' -> 'files'. An
// absent, NULL, or undecodable list answers an empty non-nil slice, which
// names no build record.
func runStateFilesOfPayload(payload json.RawMessage) []runStateFile {
	var doc struct {
		Mesh struct {
			Files json.RawMessage `json:"files"`
		} `json:"mesh"`
	}
	if len(payload) == 0 || json.Unmarshal(payload, &doc) != nil {
		return []runStateFile{}
	}
	return runStateFilesOrEmpty(doc.Mesh.Files)
}

// runStateFilesOrEmpty decodes one mesh.files value. An absent, NULL, or
// undecodable value answers an empty non-nil slice, never nil: a terminal
// cleanup reads nil as the run-state cap's "keep nothing" request alone.
func runStateFilesOrEmpty(raw []byte) []runStateFile {
	if len(raw) == 0 || string(raw) == "null" {
		return []runStateFile{}
	}
	files, err := decodeRunStateFiles(raw)
	if err != nil || files == nil {
		return []runStateFile{}
	}
	return files
}

// retainedScriptEntry answers the build-record entry a terminal cleanup
// keeps and a script read opens, and false when the list names none.
//
// Only an entry isBuildRecordPath accepts, with a positive epoch and a
// positive generation, names a build record: the object key carries both
// numbers. Among those entries the order is the highest epoch, then the
// highest generation, then the shortest path, then the lexically first
// path, so the choice never depends on list order.
func retainedScriptEntry(files []runStateFile) (runStateFile, bool) {
	var best runStateFile
	found := false
	for _, f := range files {
		if !isBuildRecordPath(f.RelPath) || f.Epoch <= 0 || f.StateGen <= 0 {
			continue
		}
		if !found || retainedBefore(f, best) {
			best = f
			found = true
		}
	}
	return best, found
}

// retainedBefore reports whether candidate orders before current under the
// retained-entry order.
func retainedBefore(candidate, current runStateFile) bool {
	if candidate.Epoch != current.Epoch {
		return candidate.Epoch > current.Epoch
	}
	if candidate.StateGen != current.StateGen {
		return candidate.StateGen > current.StateGen
	}
	if len(candidate.RelPath) != len(current.RelPath) {
		return len(candidate.RelPath) < len(current.RelPath)
	}
	return candidate.RelPath < current.RelPath
}
