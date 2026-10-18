# AGENTS.md — internal/agent

## Purpose

The core of the assistant and the team agents. It holds the session, event,
approval, and settings types, the tool policy, the data-sharing rules, the
budget checks, the fixed prompts, the session store, the model-provider
contract with its adapters, and the sandbox contract.

## Ownership

- Owns the Go types and enumerations of the agent runtime (`agent.go`,
  `payloads.go`, `sessionctx.go`, `create.go`).
- Owns the sub-packages `policy/`, `datashare/`, `budget/`, `prompts/`,
  `store/`, `provider/` (with `anthropic/`, `openaicompat/`, `providertest/`,
  `factories/`), and `sandbox/v1/` (with `none/` and `conformance/`).
- Does not own the MCP tool host, the HTTP handlers, or the agent loop. Those
  are later changes in the add-agent-runtime epic.

## Local Contracts

- **Import rule.** No non-test package under `internal/agent` imports
  `internal/mcp`, `internal/api`, or `internal/recordstore`. Test files may
  import `internal/recordstore` (for the migration harness).
- **Registry values.** Every enumeration string comes from the coordinator
  registry through `agent.go`. Do not write a literal state, kind, or code
  elsewhere.
- **Provider keys.** A provider key reaches an adapter only through
  `provider.Key.SetHeader`. `Key` prints `[redacted]` in every formatter,
  logger, and encoder.
- **Store writes.** `store/` never runs `DELETE`. It uses the `varroa_client`
  grants (`INSERT`, `UPDATE`). Every state change runs under the session row
  lock. The store reads time from `Options.Now` and never from SQL `now()`.
- **Lease timing.** `store.LeaseTTL` is 90 s. A lease holder renews every 30 s.
  A waiting session with no event for 5 min is lost.
- **Secret literals.** No Go source or test file holds a literal that matches a
  secret pattern. Build such strings from parts.
- **Egress.** No Go source holds an IP address or a host other than loopback,
  `.invalid`, or `.test`. The one exception is `DefaultBaseURL` of the
  Messages adapter, which is listed in `hack/checkoutbound/inventory.yaml`.
- **Shipping.** `cmd/fakeprovider` ships in no image.
- **Sandbox.** `sandbox/v1` never changes its exported shape. A later version
  lives beside it.

## Work Guidance

- Keep every comment in ASD-STE100. Comments state constraints, not history.
- Every exported name has a doc comment that starts with the name.
- Pass `ctx` first. Error strings start in lower case.

## Verification

```bash
go test -race -count=1 ./internal/agent/... ./cmd/fakeprovider/...
VARROA_TEST_DSN='postgres://varroa:varroa@localhost:55471/varroa?sslmode=disable' \
  go test -race -count=1 ./internal/agent/store/...
make check-outbound && make export-gate
```

The store tests skip when `VARROA_TEST_DSN` is unset. The `Record store` job in
`.github/workflows/pr.yaml` runs `./internal/agent/store/...` with the database.

## Child DOX Index

No child AGENTS.md files. The sub-packages follow the contracts above.
