# AGENTS.md — internal/teamagent

## Purpose

Team agent core. A Team agent is a `TeamAgent` object that works for one pipelines Team. This folder holds the rules that identify the agent and read its instructions. The verifier that checks the agent's token lives in `internal/auth/teamagent`. The operator ticker that writes the role bindings lives in `internal/controller/teamagents`.

## Ownership

- `accept/`: the one rule that maps each `TeamAgent` to its principal (`agent:<ns>/<team>/<name>`). It decides which object is accepted for a Team. It ignores `spec.enabled`. It imports only `api/v1alpha1`, and a test keeps it that way.
- `token/`: mints the session credential, an HS256 JWT. It reads the HMAC key file. `token.Value` never prints the token.
- `instructions/`: reads the standing instructions file through the forge relay. It maps each failed read to a condition reason.
- `cache/`: holds the `TeamAgent`, `Team`, and `CatalogSource` objects from dynamic informers. The runner and the BFF read decisions from it, never from the API server.
- `config/`: decodes the runner environment and answers `ProviderReady` for one TeamAgent.
- `store/`: the record-store queries of the runner: run and placement lookups, the spend rollup, the resume candidates, and the newest session of a principal.
- `triggers/`: the audit-feed path. `consumer.go` holds the durable `agent-triggers` consumer on `varroa_activity`. `matcher.go` decides which TeamAgent a row concerns. `create.go` writes the session row under an advisory lock keyed by agent, trigger, and item. `handoff.go` acks, naks with a 60-second delay, or starts a worker.
- `sessions/`: the session runtime. `host.go` is the HTTP MCP host. `decision.go` is the write decision. `pool.go` bounds the sessions of one replica at 8. `resume.go` takes over a session only when every tool call has its result. `killswitch.go` refreshes the settings cache on each control message.

## Local Contracts

- The principal is the only subject of an agent. The verifier sets it as the claims subject. The human login paths refuse the `agent:` prefix (`internal/auth`).
- A token names one session (`sid`). It lives 15 minutes. Its audience is `varroa-team-agent`, and it reaches only `/api/v1/mcp` and below.
- The key file must hold at least 32 bytes. A key error never prints key bytes.
- A Team agent holds one accepted object per Team. A second object of the same Team reads `Duplicate`.
- Instructions reads answer a reason from `api/v1alpha1` (`TeamAgentReason*`). A repository that pins a revision answers `NotDefaultBranch`. An OCI source answers `NotFound` with the message `source has no git repo`.
- Member-placed Teams: the forge credentials live on the member cluster. The core cannot resolve them. Their instructions read and `open_issue` answer `no_matching_credential`.
- A row that matches no accepted, enabled TeamAgent is acked and dropped. A row older than one hour is dropped.
- A create path that returns `Created` or `Skipped` publishes one content-free notice. A `Deduped` or `Debounced` answer writes no row.
- The create path reads the decision of `agent.DecideCreate`. The stored skip reason equals that decision for the same inputs (`triggers/decide_parity_test.go`).
- A trigger holds the advisory lock of its key for the whole check. Two replicas cannot both start one session.
- A pool or store failure naks the message with a 60-second delay. The pool slot is released on every path that starts no session.
- A session token never travels in a request body. The HTTP host sets it in the `Authorization` header only.
- A resume requires a result for every tool call in the transcript. A missing result blocks the resume.
- The output tools `post_note`, `open_issue`, and `send_webhook` run without approval only when the TeamAgent turns their output on.

## Work Guidance

- Put each new identity rule in `accept/`, with a table test. Keep `accept/` free of repo imports other than `api/v1alpha1`.
- Never log or format a token value. Use `token.Value` as the only carrier.
- Keep reason strings in `api/v1alpha1`. Do not add a second list here.

## Verification

- `go test ./internal/teamagent/... ./internal/auth/teamagent/... ./internal/controller/teamagents/... ./internal/forgerelay/...`
- `VARROA_TEST_DSN=<admin dsn> go test ./internal/teamagent/store/ ./internal/teamagent/triggers/` for the database tests. Each test creates and drops its own database.
- `make check-crds` after any change to `api/v1alpha1/teamagent_types.go`.

## Child DOX Index

- `accept/` — no child doc; the rules are in this file.
- `token/` — no child doc; the rules are in this file.
- `instructions/` — no child doc; the rules are in this file.
- `cache/` — no child doc; the rules are in this file.
- `config/` — no child doc; the rules are in this file.
- `store/` — no child doc; the rules are in this file.
- `triggers/` — no child doc; the rules are in this file.
- `sessions/` — no child doc; the rules are in this file.
