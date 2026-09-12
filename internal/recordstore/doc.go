// Package recordstore is the single writer of the build-record tables in
// schema "varroa" of Postgres database "varroa". It owns the migrations, the
// build-number and claim ledger, the run/stage/test/artifact record tables,
// the reader used by the BFF, and the grants a client role needs.
//
// The package holds no Jenkins plugin code, no mite code, and no gateway
// code. It holds no HTTP handler and no UI. It holds no ClickHouse or
// full-text-search code, and no job-definition or trigger state. Retention
// and partition maintenance live here, but the placement authority that
// picks a cell does not.
package recordstore
