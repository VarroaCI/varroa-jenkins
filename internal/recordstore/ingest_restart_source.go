package recordstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The caps of one restart copy. The gateway and the plugin enforce the
// same values, so a copy the recorder accepts is one the gateway stored.
const (
	RestartSourceMaxFiles = 512
	RestartSourceMaxBytes = int64(32 << 20)
)

// restartSourceFile is one file the recorder has seen of a restart copy.
type restartSourceFile struct {
	RelPath string `json:"relpath"`
	Bytes   int64  `json:"bytes"`
}

// restartSourceRecord is the manifest of one run's restart copy, stored
// under runs.payload->'restart_source'. Complete is true only when every
// declared file arrived and the byte sum matches the declared total.
type restartSourceRecord struct {
	Epoch         int64               `json:"epoch"`
	ExpectedFiles int64               `json:"expected_files"`
	ExpectedBytes int64               `json:"expected_bytes"`
	Complete      bool                `json:"complete"`
	KeptAt        time.Time           `json:"kept_at"`
	Files         []restartSourceFile `json:"files"`
}

// applyRestartSourceFile merges one restart-copy file into the run's
// manifest. A copy with a newer pusher epoch resets the manifest. An older
// epoch, or a different declared count, is ignored. A file that would push
// the copy past its declared counts is ignored too, and nothing is written.
// The envelope never fails the consumer: every ignored case returns nil.
func applyRestartSourceFile(ctx context.Context, tx pgx.Tx, intent *intentRow, env IngestEnvelope, payload RunStatePayload) error {
	if payload.RelPath == "" || payload.Bytes <= 0 || payload.PusherEpoch <= 0 ||
		payload.CopyFiles < 1 || payload.CopyFiles > RestartSourceMaxFiles ||
		payload.CopyBytes < 1 || payload.CopyBytes > RestartSourceMaxBytes {
		return nil
	}

	var raw []byte
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(payload->'restart_source', 'null'::jsonb)
		  FROM varroa.runs WHERE run_id = $1 AND created_at = $2 FOR UPDATE
	`, intent.runID, intent.createdAt).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read restart copy: %w", err)
	}

	var rec *restartSourceRecord
	if string(raw) != "null" {
		var stored restartSourceRecord
		// A manifest that does not decode is treated as absent, so the
		// copy starts again rather than failing the consumer.
		if json.Unmarshal(raw, &stored) == nil {
			rec = &stored
		}
	}
	switch {
	case rec == nil || payload.PusherEpoch > rec.Epoch:
		rec = &restartSourceRecord{
			Epoch:         payload.PusherEpoch,
			ExpectedFiles: payload.CopyFiles,
			ExpectedBytes: payload.CopyBytes,
			KeptAt:        env.OccurredAt,
		}
	case payload.PusherEpoch < rec.Epoch:
		return nil
	case payload.CopyFiles != rec.ExpectedFiles || payload.CopyBytes != rec.ExpectedBytes:
		return nil
	}

	upserted := false
	for i := range rec.Files {
		if rec.Files[i].RelPath == payload.RelPath {
			rec.Files[i].Bytes = payload.Bytes
			upserted = true
			break
		}
	}
	if !upserted {
		rec.Files = append(rec.Files, restartSourceFile{RelPath: payload.RelPath, Bytes: payload.Bytes})
	}

	var sum int64
	for _, f := range rec.Files {
		sum += f.Bytes
	}
	if int64(len(rec.Files)) > rec.ExpectedFiles || sum > rec.ExpectedBytes {
		return nil
	}
	rec.Complete = int64(len(rec.Files)) == rec.ExpectedFiles && sum == rec.ExpectedBytes

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode restart copy: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE varroa.runs
		   SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object('restart_source', $3::jsonb)
		 WHERE run_id = $1 AND created_at = $2
	`, intent.runID, intent.createdAt, data); err != nil {
		return fmt.Errorf("record restart copy: %w", err)
	}
	return nil
}

// RestartSourceFile names one file of a complete restart copy.
type RestartSourceFile struct {
	RelPath string
	Bytes   int64
}

// RestartSourceOf reports the files of a run's restart copy. It answers
// ok only when the copy is complete: every declared file is recorded, the
// byte sum matches the declared total, and the record is not partial.
func RestartSourceOf(run Run) ([]RestartSourceFile, bool) {
	if len(run.Payload) == 0 {
		return nil, false
	}
	var holder struct {
		RestartSource *restartSourceRecord `json:"restart_source"`
	}
	if err := json.Unmarshal(run.Payload, &holder); err != nil || holder.RestartSource == nil {
		return nil, false
	}
	rec := holder.RestartSource
	if !rec.Complete || int64(len(rec.Files)) != rec.ExpectedFiles {
		return nil, false
	}
	var sum int64
	out := make([]RestartSourceFile, 0, len(rec.Files))
	for _, f := range rec.Files {
		sum += f.Bytes
		out = append(out, RestartSourceFile(f))
	}
	if sum != rec.ExpectedBytes {
		return nil, false
	}
	return out, true
}
