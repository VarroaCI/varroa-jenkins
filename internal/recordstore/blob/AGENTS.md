# internal/recordstore/blob

## Purpose

The blob-storage abstraction for build logs and build artifacts, behind
one `Store` interface. `pgblob` implements it over table
`varroa.log_chunks`. A `gocloud.dev/blob` adapter implements it over S3,
GCS, Azure Blob, and the local filesystem.

## Ownership

- Owns the `Store` interface, `ObjectOptions`, the six sentinel errors,
  the `pgblob` driver, the `gocloud.dev` adapter, the `Open` URL opener,
  and the `logs/<run_id>` / `logindex/<run_id>` key helpers.
- Does NOT own the artifact key layout past its fixed prefix
  (`artifact-manager-s3` owns it) and does NOT own `run_artifacts.blob_key`
  (`internal/recordstore` owns that column).

## Local Contracts

- **Two read shapes** — `NewRangeReader` serves a chunked object Varroa
  wrote itself, one part at a time (a log). `NewObjectReader` serves a
  native object a foreign writer wrote whole (an artifact). The two never
  share a key space: `pgblob` marks a native row with `chunk_seq = -1`;
  the `gocloud.dev` adapter holds a native object at the bare key and a
  chunked part at `<key>/<seq padded to 12 digits>`.
- **`ObjectOptions.Validate`** — an empty `ObjectClass` or a zero
  `CreatedAt` returns `ErrInvalidOptions`. `CreatedAt` must be the same
  value for every part of one object, because `pgblob` uses it in the
  `log_chunks` primary key.
- **Append is write-once per `(key, seq)`** — an identical retry
  succeeds; a retry with different bytes returns `ErrChunkConflict`.
  `pgblob` checks this with `ON CONFLICT (created_at, object_key,
  chunk_seq) DO NOTHING` plus a stored-digest comparison. The
  `gocloud.dev` adapter checks it against the part's `digest` object
  metadata.
- **`Overwrite` replaces one part** — it writes `(key, seq)` over any
  stored part and stores the new digest, so `Append` stays write-once
  against the new bytes. Only the gateway log writer calls it, for a log
  part that a lost writer left above a hole. `pgblob` uses `ON CONFLICT
  … DO UPDATE` that sets `digest` and `bytes` alone, because role
  `varroa_gateway` holds `UPDATE` on those two columns and no other.
- **`Parts` lists past a hole, with sizes** — it answers every chunked
  part at or above `from` as a `Part{Seq, Size}`, holes included, where
  `NewRangeReader` stops at the first missing part. A driver answers the
  sizes from its listing and reads no part: the `gocloud.dev` adapter
  from `ListObject.Size`, `pgblob` from `octet_length(bytes)`. The log
  writer uses it to find the old parts a moved build must empty.
- **`UnindexedLogParts`** (`logparts.go`) — the one rule for the log parts
  a restarted gateway indexes: the run of parts directly above the highest
  index entry, up to the first missing part or the first part of size 0.
  The gateway's `recoverRunState`, the recorder's `StoredLogNextSeq`, and
  both manifest readers call it, and they must not compute it another way.
- **`Open`** (`open.go`) accepts `pgblob:`, `s3://`, `gs://`, `azblob://`,
  and `file://`. `pgblob:` needs a non-nil `*pgxpool.Pool`; every other
  scheme opens a `gocloud.dev/blob` bucket and ignores the pool. Every
  other scheme returns an error naming the supported set. Driver
  registration (`s3blob`, `gcsblob`, `azureblob`, `fileblob`) happens
  through blank imports in `open.go`.
- **`SignedURL`** — mints a read URL for the exact key it receives, for a
  caller-supplied lifetime, with no derived key and no part suffix.
  `pgblob` always returns `ErrUnsupported`: the Postgres tier stores
  bytes in `log_chunks` and can sign no URL. The `gocloud.dev` adapter
  delegates to the bucket's own signer, so its answer depends on how the
  caller opened that bucket (an S3 bucket signs with no extra
  configuration; a `file://` bucket needs an `Options.URLSigner`, which
  `Open` never sets).
- **Key helpers** (`keys.go`) — `LogKey` and `LogIndexKey` are the only
  place a `logs/` or `logindex/` string appears outside their own tests.
  `RunStateKey(runID, pusherEpoch, stateGen, relpath)` renders
  `runstate/<run_id>/<pusher_epoch>/<state_gen>/<relpath>`. Both numbers
  are part of the key because two gateway replicas share no order: without
  them a superseded `Replace` can land on the live object, and no rule
  inside one replica can stop that. The epoch separates two Jenkins
  processes, and the generation separates two pushes of one process, which
  a gateway rollout can leave in flight on two replicas at one moment.
  `RunStatePrefix(runID)` stays `runstate/<run_id>/`, so one
  `DeletePrefix` still removes every push of a run.
  This package adds no artifact-key helper: `artifact-manager-s3` owns
  the layout after the fixed prefix, and `run_artifacts.blob_key` records
  the exact key.
- **No import of `internal/recordstore`** anywhere in package `blob`
  (`testutil_test.go`, `pgblob_test.go`, `open_pgblob_test.go`)
  imports it for `RunMigrations`, because `internal/recordstore`'s
  production code imports `blob` for its retention key helpers. An
  in-package test file cannot import a package that imports `blob` back
  without a cycle; declaring these three `package blob_test` instead
  keeps the database-backed harness out of that cycle. A test in package
  `blob_test` reaches only exported `blob` identifiers, so
  `pgblob_test.go` writes its native row through a plain `INSERT`
  instead of the unexported `pgStore` type.
- **Conformance suite** (`conformance_test.go`) — `RunConformance` runs
  one behavior suite against any `Store`, and stays in package `blob` so
  both test packages can call it. `pgblob_test.go` (package `blob_test`)
  runs it over schema `varroa` in this binary's own database, `<db>_blob`,
  which `testutil_test.go` creates from the `VARROA_TEST_DSN` URL on first
  use (skips without the DSN). The parent package drops schema `varroa` in
  the DSN's database throughout its run, so a blob test never touches that
  database. The harness drops and recreates schema `varroa` in the blob
  database on every setup, so a run that starts months after the database
  was created still holds partitions for the current month. `bucketstore_test.go` (package `blob`) runs it over
  a `file://` bucket in a temp directory (always runs). Each driver's
  `writeNative` hook writes a native object the way a foreign writer
  would: a direct `INSERT` with `chunk_seq = -1` for `pgblob`, a direct
  `bucket.NewWriter` at the bare key for `gocloud.dev`.

## Work Guidance

- A new driver implements the full `Store` interface and passes
  `RunConformance` before anything depends on it.
- `Delete` and `DeletePrefix` report no error for a key or prefix that
  matches nothing, so a retried retention round succeeds.

## Verification

```bash
go build ./internal/recordstore/blob/...
VARROA_TEST_DSN='postgres://user:pass@host:port/db?sslmode=disable' \
  go test -race -count=1 ./internal/recordstore/blob/...
go test -race -count=1 ./internal/recordstore/blob/...   # DSN unset: pgblob skips, file:// still runs
```

## Child DOX Index

None.
