package rehydrate

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// BuildBundle assembles result into the wire RehydrationBundle
// (design.md D3): it gzips every file, computes content_hash over the
// sorted raw entries, mints a version 7 bundle_id, and fills
// reload_items, truncated, record_count, and job_count from result.
func BuildBundle(result BudgetResult) (*mitev1.RehydrationBundle, error) {
	files := append([]CandidateFile(nil), result.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	protoFiles := make([]*mitev1.RehydrationFile, 0, len(files))
	for _, f := range files {
		gz, err := gzipCompress(f.Content)
		if err != nil {
			return nil, fmt.Errorf("gzip %s: %w", f.Path, err)
		}
		protoFiles = append(protoFiles, &mitev1.RehydrationFile{
			Path:        f.Path,
			GzipContent: gz,
			RawSize:     int64(len(f.Content)),
		})
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("mint bundle id: %w", err)
	}

	reloadItems := append([]string(nil), result.JobPaths...)
	sort.Strings(reloadItems)

	return &mitev1.RehydrationBundle{
		BundleId:    id.String(),
		ContentHash: contentHash(files),
		Files:       protoFiles,
		ReloadItems: reloadItems,
		Truncated:   result.Truncated,
		RecordCount: result.RecordCount,
		JobCount:    len(result.JobPaths),
	}, nil
}

// SerializedSize answers the byte length of result's RehydrationBundle
// once it is serialized for the wire. `BusTransport.Send`
// (internal/transport/bus_transport.go) puts the whole
// `OperatorMessage` on the bus as JSON, so this measures that same
// encoding — the message's field names, the gzip content's base64
// expansion, and every path and metadata value included — rather than
// a raw sum of gzipped file payloads (design.md D3/D4: "The operator
// builds every seed, then measures the serialized bundle").
func SerializedSize(result BudgetResult) (int, error) {
	bundle, err := BuildBundle(result)
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// contentHash answers a sha256 hex digest over sortedFiles' paths and
// raw content, each length-prefixed so no concatenation of two
// different file sets can collide. It hashes the raw content, not the
// gzipped bytes: raw content does not depend on the gzip encoder, so
// the hash stays stable regardless of how the compressor behaves.
// sortedFiles SHALL already be sorted by Path.
func contentHash(sortedFiles []CandidateFile) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, f := range sortedFiles {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(f.Path)))
		h.Write(lenBuf[:])
		h.Write([]byte(f.Path))
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(f.Content)))
		h.Write(lenBuf[:])
		h.Write(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}
