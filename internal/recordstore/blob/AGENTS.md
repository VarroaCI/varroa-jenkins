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
- **Append is write-once per `(key, seq)`, enforced at commit** — an
  identical retry succeeds; a retry with different bytes returns
  `ErrChunkConflict`. A call that stalled never replaces a part another
  writer created meanwhile. `pgblob` gets this from `ON CONFLICT
  (created_at, object_key, chunk_seq) DO NOTHING` plus a stored-digest
  comparison. The `gocloud.dev` adapter writes with `IfNotExist`
  (`If-None-Match: *`). Its check before the write and its check after a
  refused write both compare the stored bytes, never the `digest`
  metadata: `fileblob` writes a refused conditional write's metadata
  anyway, so on that driver alone a lost race can leave the loser's
  `digest` and `writer` metadata. The chart never renders `fileblob`. A bucket without conditional writes
  fails every `Append`; no fallback exists. `afterCheck` is a test-only
  seam into that window.
- **`Overwrite` is a compare-and-swap on a `Stamp`** — `Overwrite(ctx,
  key, seq, p, expect, opts)` replaces the part at `(key, seq)` only while
  it still carries `expect` (`Stamp{Digest, Writer}`), and stores the new
  digest and writer, so `Append` stays write-once against the new bytes.
  A changed or absent part answers `ErrChunkConflict`. Only the gateway
  log writer calls it: for a log part a lost writer left above a hole,
  for a blank-pass empty part, and for its own index reservation.
  `pgblob` runs `UPDATE … WHERE digest = $expected AND writer =
  $expected`, which sets `digest`, `bytes`, and `writer` alone, because
  role `varroa_gateway` holds `UPDATE` on those three columns and no other.
  The `gocloud.dev` adapter compares the stamp after one attribute read,
  then writes with `If-Match` on the ETag it read. gocloud.dev has no
  `If-Match` option: `ifMatchBeforeWrite` sets it on the S3 driver's
  `transfermanager.UploadObjectInput` through `BeforeWrite`. Known limits:
  - An ETag covers the bytes alone. Between the stamp check and the write,
    a part that another writer stored with the same bytes (in practice an
    empty part) passes `If-Match`. `pgblob` checks the writer at commit.
  - MinIO creates the object when `If-Match` names an absent key; AWS S3
    refuses it. Only retention deletes a log part, after the run ended, so
    no live `Overwrite` targets an absent part.
  - `fileblob` sends no condition: the stamp check is its only guard. It is
    never deployed. `gs` and `azblob` refuse every `Overwrite`
    (`ErrOverwriteUnsupported`); the chart never renders them either.
  - `beforeOverwrite` is a test-only seam into the window between the check
    and the write.
  On S3 a stamp's digest comes from object metadata; on every other driver
  it is the digest of the stored bytes, because `fileblob` metadata can
  name a refused write's bytes.
- **`Parts` lists past a hole, with sizes** — it answers every chunked
  part at or above `from` as a `Part{Seq, Size}`, holes included, where
  `NewRangeReader` stops at the first missing part. A driver answers the
  sizes from its listing and reads no part: the `gocloud.dev` adapter
  from `ListObject.Size`, `pgblob` from `octet_length(bytes)`. The log
  writer uses it to find the old parts a moved build must empty.
- **Writer stamp** — `ObjectOptions.Writer` names a part's writer.
  `pgblob` stores it in `log_chunks.writer`, the adapter in object metadata
  `writer`; `Overwrite` replaces it. `PartStamp` reads it back with the
  part's digest (one attribute read on S3) and answers `ErrObjectNotFound`
  for an absent part. `Parts` never answers it, so a listing stays one
  call. `Digest(p)` is the `sha256:<hex>` value a stamp carries.
- **`UnindexedLogParts`** (`logparts.go`) — the one rule for the log parts
  a restarted gateway indexes: the run of parts directly above the highest
  index entry, up to the first missing part, the first part of size 0, or
  the first part whose writer differs from the baseline. The baseline is
  the writer stamp of an index reservation at `from` (the gateway's
  `reserved` line, read by `PartStamp` on `logindex/<run_id>`), else the
  caller's highest-entry writer, else the first part's. It returns that
  baseline.
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
  `bucket.NewWriter` at the bare key for `gocloud.dev`. `s3_live_test.go`
  runs the suite and the `If-Match` window against a live S3 bucket, only
  when `VARROA_TEST_S3_URL` is set; its file comment holds the port-forward
  recipe for the core cluster's MinIO.

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
VARROA_TEST_S3_URL='s3://…' go test -race -count=1 -run S3 ./internal/recordstore/blob/   # live S3; see s3_live_test.go
```

## Child DOX Index

None.
