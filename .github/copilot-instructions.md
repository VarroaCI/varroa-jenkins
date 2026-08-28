# Copilot instructions for varroa-jenkins

Varroa is a Kubernetes-native operator for managing Jenkins controllers: a Go backend
split into operator/gateway/BFF binaries, a gRPC **mite** sidecar in every Jenkins pod,
a React/TypeScript frontend, a Helm chart, and a Jenkins auth plugin under `plugin/`.

## Code review guidance

- **Greenfield — no legacy.** This project has no external userbase. Never request
  backward compatibility, deprecation paths, migration flags, or dual-shape responses.
  A breaking contract change is correct as long as every in-repo consumer (frontend,
  mite, charts, MCP tools, docs) is updated in the same PR — flag *missed consumers*,
  not the break itself.
- **Do not comment on generated code:** `api/v1alpha1/zz_generated.deepcopy.go`,
  `internal/mite/proto/mitev1/`, `pkg/client/`, `charts/varroa/crds/`, and
  `charts/varroa/templates/version-profiles/` are all generated.
- **Style is linter-owned.** golangci-lint v2 (revive, gofmt, goimports) gates every
  PR; don't flag formatting, naming, or import ordering the linter accepts. Import
  groups intentionally place the local prefix `github.com/varroaci/varroa-jenkins` last.
- Go tests run under the race detector in CI. Concurrency findings should identify a
  concrete interleaving (which goroutines, which shared state), not generic "consider
  adding a mutex" advice.
- **Docs ship with features.** A behavior change that doesn't touch `docs/` is a valid
  finding; a docs-only PR needing no code is normal.
- **Public docs describe current product behavior only.** Flag private identifiers,
  deprecated Varroa contracts, source-level implementation detail, test-campaign
  narration, and duplicated generated API or CLI reference material.
- **Workflows run on GitHub-hosted `ubuntu-latest`.** A workflow step invoking a CLI
  that neither the runner image nor `./.github/actions/setup-ci-tools` provides is a
  valid finding. Prefer extending that composite action over adding an inline
  installer, and pin tool versions rather than tracking `latest` when a script asserts
  a specific major version.
- Prioritize correctness in the hot paths: reconcile-loop idempotency and requeue
  behavior, status patches (merge patches cannot clear `omitempty` fields), gRPC
  stream lifecycle/keepalive in `internal/mite/` + `cmd/mite/`, JCasC bundle merge
  order, and RBAC strategy generation.
- Skip compliments, diff restatements, and speculative "you might also want to"
  comments — every comment should be actionable as written.
