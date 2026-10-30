// Flow-node stage attribution (run-log-viewer, "A pipeline step line
// SHALL join the stage of its flow node"). Jenkins writes a
// NewNodeConsoleNote at the start of each [Pipeline] line that the
// overall listener prints. That chunk carries no nodeId and no stageId,
// so the note is the only source of the line's stage. This file decodes
// the note's three fields and walks them to the nearest labelled block.
//
// The decoder reads one fixed Java serialization layout. A different
// class header leaves the line unattributed. The MAC that Jenkins may
// place before the payload is skipped and never checked: the result only
// groups display rows, and only overall-listener chunks are read.
package api

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"
	"regexp"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// newNodeNotePrefix is the exact Java serialization header of
// NewNodeConsoleNote up to its field values. A different header means a
// different Jenkins class layout, and the line stays unattributed.
var newNodeNotePrefix = mustHex("aced00057372003d6f72672e6a656e6b696e7363692e706c7567696e732e776f726b666c6f772e6a6f622e636f6e736f6c652e4e65774e6f6465436f6e736f6c654e6f746500000000000000010200034c0009656e636c6f73696e677400124c6a6176612f6c616e672f537472696e673b4c0002696471007e00014c0005737461727471007e00017872001a687564736f6e2e636f6e736f6c652e436f6e736f6c654e6f746500000000000000010200007870")

const (
	// maxNoteBase64Bytes bounds the base64 run after the preamble.
	maxNoteBase64Bytes = 4096
	// maxNoteGunzipBytes bounds the decompressed note body.
	maxNoteGunzipBytes = 4096
	// maxNoteStringBytes bounds one serialized string value.
	maxNoteStringBytes = 128
	// maxStageParentEntries bounds the parent map of one attributor.
	maxStageParentEntries = 200000
	// maxStageWalkSteps bounds the walk from a node to its block.
	maxStageWalkSteps = 256

	// firstNoteStringHandle is the handle the first string value of a
	// note takes. The class descriptor uses the handles below it.
	firstNoteStringHandle = 0x7e0004

	tcNull      = 0x70
	tcString    = 0x74
	tcReference = 0x71
)

// mustHex decodes a hex literal that is fixed at build time.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("runlog_stage_notes: bad hex literal: " + err.Error())
	}
	return b
}

// newNodeNote holds the three flow-node fields of one note. An empty
// string means the Java value is null.
type newNodeNote struct{ ID, Enclosing, Start string }

// consoleNotePayloadAtLineStart returns the base64 payload of a console
// note that starts raw, or "" when raw does not start with one, the run
// is empty, or the run is longer than maxNoteBase64Bytes.
func consoleNotePayloadAtLineStart(raw []byte) string {
	pre := []byte(consoleNotePreamble)
	if !bytes.HasPrefix(raw, pre) {
		return ""
	}
	k := len(pre)
	for k < len(raw) && isConsoleNoteBase64Byte(raw[k]) {
		k++
		if k-len(pre) > maxNoteBase64Bytes {
			return ""
		}
	}
	if k == len(pre) {
		return ""
	}
	return string(raw[len(pre):k])
}

// decodeNewNodeNote reads the note payload. It returns false for any
// byte that does not match the fixed layout, for a bad base64 run, and
// for a body that is not gzip.
func decodeNewNodeNote(payload string) (newNodeNote, bool) {
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return newNodeNote{}, false
	}
	i := 0
	readInt := func() (int32, bool) {
		if len(b)-i < 4 {
			return 0, false
		}
		v := int32(binary.BigEndian.Uint32(b[i : i+4]))
		i += 4
		return v, true
	}
	n, ok := readInt()
	if !ok {
		return newNodeNote{}, false
	}
	if n < 0 {
		macLen := -int64(n)
		if macLen > int64(len(b)-i) {
			return newNodeNote{}, false
		}
		i += int(macLen)
		if n, ok = readInt(); !ok {
			return newNodeNote{}, false
		}
	}
	if n <= 0 || int64(n) > int64(len(b)-i) {
		return newNodeNote{}, false
	}
	zr, err := gzip.NewReader(bytes.NewReader(b[i : i+int(n)]))
	if err != nil {
		return newNodeNote{}, false
	}
	defer func() { _ = zr.Close() }()
	z, err := io.ReadAll(io.LimitReader(zr, maxNoteGunzipBytes+1))
	if err != nil || len(z) > maxNoteGunzipBytes {
		return newNodeNote{}, false
	}
	if !bytes.HasPrefix(z, newNodeNotePrefix) {
		return newNodeNote{}, false
	}
	return readNewNodeNoteValues(z[len(newNodeNotePrefix):])
}

// readNewNodeNoteValues reads the three field values in the order
// enclosing, id, start. A string value takes the next handle. A
// reference names a string that this note already read.
func readNewNodeNoteValues(v []byte) (newNodeNote, bool) {
	handles := map[uint32]string{}
	next := uint32(firstNoteStringHandle)
	i := 0
	readValue := func() (string, bool) {
		if i >= len(v) {
			return "", false
		}
		tag := v[i]
		i++
		switch tag {
		case tcNull:
			return "", true
		case tcString:
			if len(v)-i < 2 {
				return "", false
			}
			size := int(binary.BigEndian.Uint16(v[i : i+2]))
			i += 2
			if size > maxNoteStringBytes || len(v)-i < size {
				return "", false
			}
			s := string(v[i : i+size])
			i += size
			handles[next] = s
			next++
			return s, true
		case tcReference:
			if len(v)-i < 4 {
				return "", false
			}
			h := binary.BigEndian.Uint32(v[i : i+4])
			i += 4
			s, known := handles[h]
			return s, known
		}
		return "", false
	}
	var out newNodeNote
	var ok bool
	if out.Enclosing, ok = readValue(); !ok {
		return newNodeNote{}, false
	}
	if out.ID, ok = readValue(); !ok {
		return newNodeNote{}, false
	}
	if out.Start, ok = readValue(); !ok {
		return newNodeNote{}, false
	}
	return out, true
}

// labelledBlockLine matches the text of a block start that carries a
// label, as the plugin's walkToRecordedParent expects it.
var labelledBlockLine = regexp.MustCompile(`^\[Pipeline\] (?:\[[^\]]*\] )?\{ \(.+\)$`)

// runLogStageAttributor maps each flow node to its enclosing node and
// records which nodes are labelled blocks. One attributor serves one
// run log, so the history replay and the live path share it.
type runLogStageAttributor struct {
	parent   map[string]string
	labelled map[string]bool
}

// newRunLogStageAttributor starts an attributor that knows every stage
// of the run as a labelled block.
func newRunLogStageAttributor(stages []recordstore.Stage) *runLogStageAttributor {
	a := &runLogStageAttributor{parent: map[string]string{}, labelled: map[string]bool{}}
	for _, st := range stages {
		if len(a.labelled) >= maxStageParentEntries {
			break
		}
		a.labelled[st.StageID] = true
	}
	return a
}

// attribute sets ln.StageID from the line's flow-node note. It records
// the note's enclosing link and its block label first, so a later line
// can walk through this node. It is nil-safe, and it does nothing for a
// line with no note payload. A line whose chunk already named a stage
// keeps that stage, but its note still feeds the walk state.
func (a *runLogStageAttributor) attribute(ln *runLogLine) {
	if a == nil || ln.notePayload == "" {
		return
	}
	note, ok := decodeNewNodeNote(ln.notePayload)
	if !ok {
		return
	}
	isEnd := note.Start != "" && note.Start != note.ID
	if !isEnd {
		if note.Enclosing != "" && len(a.parent) < maxStageParentEntries {
			a.parent[note.ID] = note.Enclosing
		}
		if note.Start == note.ID && len(a.labelled) < maxStageParentEntries &&
			labelledBlockLine.MatchString(strings.TrimSuffix(ln.Text, "\r")) {
			a.labelled[note.ID] = true
		}
	}
	if ln.StageID != "" {
		return
	}
	from := note.ID
	if isEnd {
		from = note.Start
	}
	ln.StageID = a.nearest(from)
}

// nearest walks from id through the enclosing links and returns the
// first labelled node, or "" when none is found within the step bound.
func (a *runLogStageAttributor) nearest(id string) string {
	x := id
	for step := 0; step < maxStageWalkSteps && x != ""; step++ {
		if a.labelled[x] {
			return x
		}
		x = a.parent[x]
	}
	return ""
}
