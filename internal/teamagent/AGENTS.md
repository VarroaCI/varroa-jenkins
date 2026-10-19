# AGENTS.md — internal/teamagent

## Purpose

Team agent core. A Team agent is a `TeamAgent` object that works for one pipelines Team. This folder holds the rules that identify the agent and read its instructions. The verifier that checks the agent's token lives in `internal/auth/teamagent`. The operator ticker that writes the role bindings lives in `internal/controller/teamagents`.

## Ownership

- `accept/`: the one rule that maps each `TeamAgent` to its principal (`agent:<ns>/<team>/<name>`). It decides which object is accepted for a Team. It ignores `spec.enabled`. It imports only `api/v1alpha1`, and a test keeps it that way.
- `token/`: mints the session credential, an HS256 JWT. It reads the HMAC key file. `token.Value` never prints the token.
- `instructions/`: reads the standing instructions file through the forge relay. It maps each failed read to a condition reason.

## Local Contracts

- The principal is the only subject of an agent. The verifier sets it as the claims subject. The human login paths refuse the `agent:` prefix (`internal/auth`).
- A token names one session (`sid`). It lives 15 minutes. Its audience is `varroa-team-agent`, and it reaches only `/api/v1/mcp` and below.
- The key file must hold at least 32 bytes. A key error never prints key bytes.
- A Team agent holds one accepted object per Team. A second object of the same Team reads `Duplicate`.
- Instructions reads answer a reason from `api/v1alpha1` (`TeamAgentReason*`). A repository that pins a revision answers `NotDefaultBranch`. An OCI source answers `NotFound` with the message `source has no git repo`.
- Member-placed Teams: the forge credentials live on the member cluster. The core cannot resolve them. Their instructions read and `open_issue` answer `no_matching_credential`.

## Work Guidance

- Put each new identity rule in `accept/`, with a table test. Keep `accept/` free of repo imports other than `api/v1alpha1`.
- Never log or format a token value. Use `token.Value` as the only carrier.
- Keep reason strings in `api/v1alpha1`. Do not add a second list here.

## Verification

- `go test ./internal/teamagent/... ./internal/auth/teamagent/... ./internal/controller/teamagents/... ./internal/forgerelay/...`
- `make check-crds` after any change to `api/v1alpha1/teamagent_types.go`.

## Child DOX Index

- `accept/` — no child doc; the rules are in this file.
- `token/` — no child doc; the rules are in this file.
- `instructions/` — no child doc; the rules are in this file.
