# AGENTS.md

This file is the root contract for agents and contributors. Several subtrees have their own `AGENTS.md`; the index at the end lists them. Read this file first. Then read every `AGENTS.md` on the path to each file you change. The closest file controls local detail. No child file may weaken a rule here.

## What This Project Is

Varroa is a Kubernetes-native operator that manages Jenkins controllers at scale. It has these parts:

- A Go backend split into six binaries. `varroa-operator` reconciles the CRDs. `varroa-gateway` serves mite gRPC on `:9090`. `varroa-bff` serves the HTTP API and SSE on `:8080`. `varroa-updatecenter` is an opt-in in-cluster plugin update center. `varroa-recorder` is the opt-in build record store. `varroa-agent` runs Tenant agent sessions.
- A **mite** sidecar (`cmd/mite/`) in every Jenkins pod. It receives commands over gRPC with mTLS.
- A React/TypeScript dashboard (`frontend/`).
- A Helm chart (`charts/varroa/`) that deploys Varroa, Dex, and NATS. You bring your own observability stack.
- A `VarroaSecurityRealm` Jenkins plugin (`plugin/`). The image carries it, and an init container delivers it to each Jenkins pod.
- `varroactl`, a CLI with an MCP bridge for agents.

Two controller shapes exist. A classic controller is one Jenkins per `Controller` CR. A cell controller (`spec.cell.enabled`) is a shared, disposable Jenkins. It hosts several `Tenant` tenants and keeps build history in the record store.

## Project Principles

- **Prefer mature open-source tooling.** Evaluate an existing Jenkins or Kubernetes solution before you build a Varroa-specific one.
- **Greenfield, no legacy.** Varroa has no external userbase. Break contracts and roll forward. Add no deprecation path, no dual-shape response, no back-compat shim, and no migration flag. When a contract changes, update every in-repo consumer in the same change. Consumers are the frontend, the MCP tools, the mite, the charts, and the docs.
- **Docs ship with features.** A feature change updates the user-facing docs under `docs/` in the same PR. A feature is not done while the docs describe the old behavior.
- **Write in Simplified Technical English.** This applies to docs, code comments, commit messages, and PR text. Use one instruction per sentence, active voice, and present tense. Keep sentences at or under 20 words.
- **Comments carry constraints, not history.** A Go comment may state a constraint the code cannot show. It may not cite an issue or PR number, narrate the next line, or argue with a reviewer. `make check-comments` blocks citations on every PR.

## Commands

```bash
make build          # the six backend binaries into bin/ (not varroa-agent or varroactl)
make test           # all Go tests with the race detector
make lint           # golangci-lint
make chart-test     # chart golden tests (after helm dependency build charts/varroa)
make check-comments # the comment rule
make localdev       # full local stack on kind

go test -v -race -count=1 ./internal/bundle/...   # one package

cd frontend && npm ci && npm run dev   # Vite dev server at :3000
```

Set `VITE_VARROA_BFF_URL=http://localhost:8080/api/v1` in `frontend/.env` to point the dashboard at a local BFF.

## Repo-wide Rules

- **Hand-write DeepCopy.** `api/v1alpha1/zz_generated.deepcopy.go` is maintained by hand for every kind. Never run `controller-gen object`. See `api/AGENTS.md`.
- **One decision point per derived fact.** `v1alpha1.EffectiveBundleRef` alone resolves a nil `spec.composedBundleRef`. `v1alpha1.IsHeadless` alone decides whether a controller blocks its web UI. Do not read those fields directly elsewhere. See `api/AGENTS.md` and `internal/controller/AGENTS.md`.
- **The desired-state command carries credential values.** It travels only as a core NATS request. No KV bucket or stream may store it. See `internal/mite/AGENTS.md`.
- **Only audit-worthy events go on `activity.*`.** Heartbeat and snapshot telemetry rides `events.brood.>`. It must never enter the bounded activity stream. See `internal/api/AGENTS.md`.
- **golangci-lint is pinned in three places.** They are `.github/workflows/pr.yaml`, `.github/workflows/release.yaml`, and `GOLANGCI_LINT_VERSION` in the `Makefile`. Bump all three in one PR and fix the resulting findings in the same change.
- **Import order.** The local prefix `github.com/varroaci/varroa-jenkins` goes last in import groups. Lint excludes the proto-generated code under `internal/mite/proto/mitev1/`.
- **Frontend commands run from `frontend/`.** Do not assume the shell is already there.
- **Wait on cluster state with a bounded poll**, not a fixed sleep: `for i in $(seq 1 30); do <check> && break; sleep 5; done`.

## CI

GitHub Actions (`.github/workflows/`) run on GitHub-hosted `ubuntu-latest` runners.

`pr.yaml` runs lint, the **Checks** job, the chart golden tests, and govulncheck. It also runs the race tests, the frontend tests, and the Docker builds. The Checks job runs the comment, outbound, generated-file, proto, API client, challenge bundle, bootstrap closure, CRD, and localdev checks. The `Detect changes` job sets the outputs `go`, `charts`, `docker`, `frontend`, and `outbound`. Each build or test job gates itself with `if:` on those outputs. There is no workflow-level paths filter. A draft pull request skips every other job, and `Required checks` fails on a draft. `Required checks` is the single aggregate context that branch protection requires.

`release.yaml` runs on a tag push, or on a manual dispatch against a tag ref. It publishes the images, the chart, the `varroactl` binaries, and the plugin seed packs. `release-please.yaml` cuts releases from conventional commits.

Never assume a CLI is on the runner image. Pull it in with `uses: ./.github/actions/setup-ci-tools`. Its inputs are `yq`, `gh`, `rg`, `make`, `helm`, `shellcheck`, `envsubst`, `python3`, and `jq`. Extend that action rather than add an inline installer.

Copilot code review reads `.github/copilot-instructions.md`, and also ingests `AGENTS.md` + `CLAUDE.md` as custom instructions. A same-repo pull request loads these files from its head branch. A fork pull request loads them from the base branch. Copilot review runs as an Actions run under a per-account model rate limit. The review body hides that error. The run log of the `copilot-pull-request-reviewer` job shows the real error. When repo conventions change here, keep `copilot-instructions.md` aligned so review comments stay low-noise.

## Updating These Files

Update the closest `AGENTS.md` after a change that affects a subtree's purpose, contracts, workflows, inputs, outputs, or constraints. Update the parent when its child index changes. Delete stale text; do not mark it stale. The default section order of a child file is Purpose, Ownership, Local Contracts, Work Guidance, Verification, and Child DOX Index. A child omits a section it does not need.

## Child DOX Index

- [api](api/AGENTS.md): CRD Go types (`v1alpha1`, group `varroa.dev/v1alpha1`) and the OpenAPI 3 contract with its client codegen.
- [cmd](cmd/AGENTS.md): the binaries. Indexes [mite](cmd/mite/AGENTS.md) and [varroactl](cmd/varroactl/AGENTS.md).
- [internal](internal/AGENTS.md): the Go backend packages hub. Indexes `controller`, `api`, `mite`, `bundle`, `rbac`, `bus`, `recordstore`, `agent`, and `tenantagent`.
- [bundles](bundles/AGENTS.md): JCasC content embedded in the operator binary, including the built-in starter bundle.
- [pkg](pkg/AGENTS.md): the consumable Go surface: generated OpenAPI client, stream helper, embedded templates.
- [frontend](frontend/AGENTS.md): the React/TypeScript/Vite dashboard and its test and build gates.
- [charts/varroa](charts/varroa/AGENTS.md): the umbrella Helm chart and its generated CRDs.
- [plugin](plugin/AGENTS.md): the `VarroaSecurityRealm` Jenkins plugin (Maven/HPI, JDK 21).

These directories have no child file:

- `docs/`: the user handbook, published at https://varroa.dev/docs/.
- `examples/`: sample CRs.
- `config/`: reference manifests.
- `assets/`: static images.
- `scripts/`: small helper scripts.
- `legal/`: the commercial license template.
