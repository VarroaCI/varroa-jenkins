# AGENTS.md

This file is the guide for agents and contributors. Read it before you change the code.

## What This Project Is

Varroa is a Kubernetes-native operator that manages Jenkins controllers at scale. It has these parts:

- A Go backend split into six binaries. `varroa-operator` reconciles the CRDs. `varroa-gateway` serves mite gRPC on `:9090`. `varroa-bff` serves the HTTP API and SSE on `:8080`. `varroa-updatecenter` is an opt-in in-cluster plugin update center. `varroa-recorder` is the opt-in build record store. `varroa-agent` runs Tenant agent sessions.
- A **mite** sidecar (`cmd/mite/`) in every Jenkins pod. It receives commands over gRPC with mTLS.
- A React/TypeScript dashboard (`frontend/`).
- A Helm chart (`charts/varroa/`) that deploys Varroa, Dex, and NATS. You bring your own observability stack.
- A `VarroaSecurityRealm` Jenkins plugin (`plugin/`). The image carries it, and an init container delivers it to each Jenkins pod.
- `varroactl`, a CLI with an MCP bridge for agents.

Two controller shapes exist. A classic controller is one Jenkins per `Controller` CR. A cell controller (`spec.cell.enabled`) is a shared, disposable Jenkins. It hosts several tenants and keeps build history in the record store.

## Repository Layout

- `api/`: the CRD Go types (`v1alpha1`, group `varroa.dev/v1alpha1`) and the OpenAPI 3 contract.
- `cmd/`: the binaries, including the mite and `varroactl`.
- `internal/`: the Go backend packages: controller, API, mite gateway, bundles, RBAC, bus, record store, and agents.
- `bundles/`: JCasC content embedded in the operator binary, including the built-in starter bundle.
- `pkg/`: the consumable Go surface: the generated OpenAPI client, a stream helper, and embedded templates.
- `frontend/`: the React/TypeScript/Vite dashboard.
- `charts/varroa/`: the umbrella Helm chart and its generated CRDs.
- `plugin/`: the `VarroaSecurityRealm` Jenkins plugin (Maven/HPI, JDK 21).
- `docs/`: the user handbook, published at https://varroa.dev/docs/.
- `examples/`: sample CRs.
- `config/`: reference manifests.
- `assets/`: static images.
- `scripts/`: small helper scripts.
- `legal/`: the commercial license template.

## Project Principles

- **Prefer mature open-source tooling.** Evaluate an existing Jenkins or Kubernetes solution before you build a Varroa-specific one.
- **Change a contract in one pull request.** A contract is an API path, a payload shape, a CRD field, or a CLI flag. When one changes, update every in-repo consumer in the same pull request. Consumers are the frontend, the MCP tools, the mite, the charts, the generated client, and the docs.
- **Docs ship with features.** A feature change updates the user-facing docs under `docs/` in the same pull request. A feature is not done while the docs describe the old behavior.
- **Write in Simplified Technical English.** This applies to docs, code comments, commit messages, and pull request text. Use one instruction per sentence, active voice, and present tense. Keep sentences at or under 20 words.
- **Comments carry constraints, not history.** A Go comment may state a constraint the code cannot show. It may not cite an issue or pull request number, narrate the next line, or argue with a reviewer. `make check-comments` blocks citations on every pull request.

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

- **Hand-write DeepCopy.** `api/v1alpha1/zz_generated.deepcopy.go` is maintained by hand for every kind. Never run `controller-gen object`.
- **Do not edit generated code. Regenerate it.** `make generate-client` regenerates the OpenAPI bundle and `pkg/client/`. The proto code under `internal/mite/proto/mitev1/` and the CRDs under `charts/varroa/crds/` are generated too. `api/v1alpha1/zz_generated.deepcopy.go` is the one exception: it is maintained by hand.
- **One decision point per derived fact.** `v1alpha1.EffectiveBundleRef` alone resolves a nil `spec.composedBundleRef`. `v1alpha1.IsHeadless` alone decides whether a controller blocks its web UI. Do not read those fields directly elsewhere.
- **The desired-state command carries credential values.** It travels only as a core NATS request. No KV bucket or stream may store it.
- **Only audit-worthy events go on `activity.*`.** Heartbeat and snapshot telemetry rides `events.brood.>`. It must never enter the bounded activity stream.
- **golangci-lint is pinned in three places.** They are `.github/workflows/pr.yaml`, `.github/workflows/release.yaml`, and `GOLANGCI_LINT_VERSION` in the `Makefile`. Bump all three in one pull request and fix the resulting findings in the same change.
- **Import order.** The local prefix `github.com/varroaci/varroa-jenkins` goes last in import groups. Lint excludes the proto-generated code under `internal/mite/proto/mitev1/`.
- **Frontend commands run from `frontend/`.** Do not assume the shell is already there.
- **Wait on cluster state with a bounded poll**, not a fixed sleep: `for i in $(seq 1 30); do <check> && break; sleep 5; done`.

## CI

GitHub Actions (`.github/workflows/`) run on GitHub-hosted `ubuntu-latest` runners.

`pr.yaml` runs lint, the **Checks** job, the chart golden tests, and govulncheck. It also runs the race tests, the frontend tests, and the Docker builds. The Checks job runs the comment, outbound, generated-file, proto, API client, challenge bundle, bootstrap closure, CRD, and localdev checks. The `Detect changes` job sets the outputs `go`, `charts`, `docker`, `frontend`, and `outbound`. Each build or test job gates itself with `if:` on those outputs. There is no workflow-level paths filter. A draft pull request skips every other job, and `Required checks` fails on a draft. `Required checks` is the single aggregate context that branch protection requires.

`release.yaml` runs on a tag push, or on a manual dispatch against a tag ref. It publishes the images, the chart, the `varroactl` binaries, and the plugin seed packs. `release-please.yaml` cuts releases from conventional commits.

Never assume a CLI is on the runner image. Pull it in with `uses: ./.github/actions/setup-ci-tools`. Its inputs are `yq`, `gh`, `rg`, `make`, `helm`, `shellcheck`, `envsubst`, `python3`, and `jq`. Extend that action rather than add an inline installer.

Copilot code review reads `.github/copilot-instructions.md` and this file. A same-repo pull request loads them from its head branch. A fork pull request loads them from the base branch. When repo conventions change here, keep `copilot-instructions.md` aligned so review comments stay low-noise.
