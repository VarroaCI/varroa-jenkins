// Package blob abstracts the storage of build logs and build artifacts
// behind gocloud.dev/blob. It holds the Postgres-backed driver ("pgblob",
// storing chunks in table "varroa.log_chunks") for tier one, and it holds
// the S3/GCS/Azure/file wiring for tier two through the stock
// gocloud.dev/blob drivers.
//
// The package holds no HTTP handler and no ingest logic. It reads and
// writes bytes under a key. The caller decides what a key means, except
// for the two log keys, which LogKey and LogIndexKey fix.
package blob
