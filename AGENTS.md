# AGENTS.md

## What This Project Is

Varroa is a Kubernetes-native operator for managing Jenkins controllers at scale:
- A Go backend that is both a REST API server and a Kubernetes operator
- A **mite** sidecar (`cmd/mite/`) in every Jenkins pod, receiving commands over gRPC mTLS
- A React/TypeScript frontend dashboard (`frontend/`)
- A Helm chart (`charts/varroa/`) deploying Varroa + Dex + NATS (bring your own observability stack)
- A **VarroaSecurityRealm** Jenkins plugin in `plugin/` (built into the image and delivered to each Jenkins pod via init container) for OIDC JWT + mite operator-JWT auth

## Project Principles

- Prefer popular open-source and cloud-native tooling for Jenkins and Kubernetes problems before building custom Varroa-specific mechanisms. When a mature ecosystem solution exists, evaluate it first and only build our own when there is a clear gap or integration need.
- **Greenfield, no legacy.** Varroa has no external userbase. Break contracts and roll forward: no deprecation paths, no dual-shape responses, no back-compat shims or migration flags. When a contract changes (API paths, payload shapes, CRD fields, CLI flags), update every in-repo consumer (frontend, MCP tools, mite, charts, docs) in lockstep within the same change.
- **Docs ship with features.** Every feature change updates the user-facing docs under `docs/` (getting-started, admin-guide, operator-guide, api-keys, …) in the same change/PR that introduces the behavior. A feature is not done while the docs describe the old behavior.

## Commands

```bash
make build          # compile bin/varroa-jenkins
make test           # all Go tests with race detector
make lint           # golangci-lint
make localdev       # full local stack on kind

go test -v -race -count=1 ./internal/bundle/...   # single package

cd frontend && npm ci && npm run dev   # Vite dev server at :3000
```

Backend components are split: `varroa-operator` reconciles controllers, `varroa-gateway` serves mite gRPC on `:9090`, `varroa-bff` serves HTTP API/SSE on `:8080`, and `varroa-updatecenter` (opt-in, `updateCenter.enabled`) serves the in-cluster Jenkins plugin update center on `:8080`. Set `VITE_VARROA_BFF_URL=http://localhost:8080/api/v1` in `frontend/.env` (or export `VARROA_API_URL` for the Vite `/api` proxy) to point the UI at a local BFF.

## Architecture Map

- **Controller reconciler**: `cmd/operator` registers the controller-runtime reconciler for `Controller` CRDs. Phases: `Pending → Provisioning → Running → Connected → Failed`. The reconciler persists results via `ResourceClient.PatchControllerStatus` and publishes desired state through NATS.
- **RBAC federation**: `internal/rbac/` generates role-strategy YAML from `JenkinsRole`/`JenkinsRoleBinding` CRDs + `VarroaRole.JenkinsRoleRef` + legacy `VarroaRole.JenkinsPermissions`. `JenkinsRole` holds pure Jenkins permission sets (Global/Item/Agent); `JenkinsRoleBinding` assigns them to subjects with optional scope. Varroa owns the authorization strategy; bundle `authorizationStrategy` keys are stripped. Built-in roles (`varroa:admin`, `varroa:operator`, `varroa:developer`, `varroa:viewer`, `varroa:system-mite`) are reconciled by `role_controller.go`.
- **mite ↔ gateway ↔ NATS ↔ operator**: `cmd/gateway` runs the `internal/mite/` gRPC server. Mites register with a bootstrap HMAC token (Secret `<controller>-bootstrap`), then get an mTLS cert from `internal/ca/`. `CommandStream` is a long-lived bidi stream; the gateway bridges mite heartbeats/snapshots/results and operator commands through NATS.
- **ResourceClient + crdstore**: CRD reads/writes go through `internal/crdstore`: a small unstructured-level `Backend` seam (implemented by `ClientsetClient` and an in-memory `Fake` for tests) with typed generic helpers (`crdstore.Get[T]`/`List[T]`/`Apply[T]`/`PatchStatus[T]`, GVRs from a static registry). `ResourceClient` (`controller_controller.go`, ~40 methods) keeps only genuinely deep k8s operations (StatefulSet/Service/Ingress/Secret/ConfigMap builders, pod ops) plus named special-semantics methods (`PatchControllerStatus`, `ApplyControllerSpecSSA`, `SetHibernated`, `ClearUserPassword`); real impl `ClientsetClient` (`clientset_client.go`). Adding a CRD kind means one registry entry, never new interface methods.
- **Activity persistence**: bounded JetStream stream `varroa_activity` on subjects `activity.<cluster>.<ns>.<ctrl>` / `activity.<cluster>._global`. Backfill reads from the stream (ephemeral consumer, end-anchored); live SSE via `BusFanout` (subscribe `events.brood.>` + `activity.>`). Retention dial: `off|7d|30d|90d`. Off mode falls back to in-memory ring per BFF replica; **every reader (REST and MCP alike) goes through `deps.Backfill`, never the ring directly**. The ring is only fed in off mode. Only audit-worthy events are published on `activity.*`: mite heartbeat/snapshot telemetry rides `events.brood.>` for live consumers and must never enter the bounded stream (it evicts real audit history within days).
- **JCasC bundles**: `internal/bundle/` implements the unified `ComposedBundle` model. A `ComposedBundle` has ordered `inputs[]`, each a union of `itemRef` (catalog item), `gitSource` (git repo), or `ociSource` (OCI artifact snapshot; `CatalogSource` likewise accepts `ociRef`, exactly-one-of with `repoURL`). `ReconcileComposedBundle` (`catalog_controller.go`) materializes git inputs via `Resolver.Materialize`, OCI inputs via `MaterializeOCI` (digest-HEAD cache invalidation instead of `git ls-remote`), catalog items via `CatalogItem` CRDs, then `Composer.Compose` merges all content in order and stores the **unresolved** result in an owner-referenced `ConfigMap` (`status.contentRef`). The controller hot path reads this ConfigMap, injects `varroa_controller_*` vars via `ResolveVars`, and runs a completeness check. **`spec.composedBundleRef` is optional**: a nil ref resolves to the operator-seeded `varroa-starter` bundle in the operator namespace (`internal/controller/starter.go`, content in `bundles/`). `Reconciler.effectiveBundleRef` is the single place that decides this. Dereferencing `cr.Spec.ComposedBundleRef` directly anywhere else reintroduces a nil panic.
- **Provisioning**: `handleProvisioning` builds the init ConfigMap (Groovy startup script), StatefulSet (`CreateStatefulSet`, CreateOrUpdate semantics), agent RBAC, and reads cluster-scoped `ProvisioningDefaults` (`varroa-defaults`) unconditionally each tick.
- **Version profiles**: `JenkinsVersionProfile` (cluster-scoped CRD) pins a plugin set + a version-specific JCasC overlay + wizard catalog metadata (`channel`/`recommended`/`eol`) to a Jenkins version or LTS line. `versionprofile_controller.go` (ticker reconciler) materializes `spec.pluginSetRef` into an owner-referenced `<name>-pluginset` ConfigMap (`status.contentRef`, `PluginSetReady`) and sets a non-blocking `LockJcascMismatch` warning. `ResolveProfile` (`versionresolve.go`) selects a profile for `cr.Spec.Version` by **exact → LTS-line (`major.minor`) → embedded `pluginlock` baseline**; `resolveCoreSet`/`coreSetForCr` feed the pinned set into `managedPluginLines` (provisioning **and** connected-phase drift checks). The profile's `spec.jcasc.content` is merged onto the composed `jenkins.yaml` via `bundle.MergeJenkinsYAML` in `resolveBundleForController` **before** `ResolveVars`. Default profiles ship via `charts/varroa/templates/version-profiles/` (generated by `hack/gen-plugin-lock.sh`).
- **Update center**: `UpdateCenter` cluster-scoped singleton CR (`varroa-update-center`) + the `varroa-updatecenter` component: an in-cluster Jenkins update center serving `update-center.actual.json` + `.hpi` blobs for the exact plugin set Varroa pins, backed by `internal/oci` (local OCI-layout PVC or external registry), with sha256-verified pull-through from `updates.jenkins.io` (upstream sha256s are base64). Operator gap-reconciles declared-vs-stored plugins and rewires the plugins-init `JENKINS_UC*` env (precedence: explicit `ProvisioningDefaults` > in-cluster UC > upstream; online fallback `UpdateCenterFallback`, air-gap block `WaitingForUpdateCenter`). Populated via seed packs (ghcr), pull-through, or `varroactl export/import` (`uc://` needs `VARROACTL_UC_TOKEN`). **Pull-through resolves sha256 from update-center metadata first and falls back to the artifact-archive `.sha256` sidecar on `repo.jenkins-ci.org`, so an aged LTS-line pin is servable**, but only through that second host, which `updateCenter.pullThrough.archiveURL` can disable and a narrowed egress allowlist can block. Air-gapped or egress-restricted installs still need the pins pre-seeded via `spec.seed.refs` or `varroactl export/import`.
- **CRDs**: `api/v1alpha1/types.go`, group `varroa.dev/v1alpha1`: `Controller`, `Group`, `TemplateCatalog`, `ProvisioningDefaults`, `JenkinsVersionProfile`, `UpdateCenter`, plus `PodTemplate`/`BuildMetric`/`User`. DeepCopy in `zz_generated.deepcopy.go` is generated; do not edit.

## Gotchas (non-obvious, won't survive a code read)

- **gRPC keepalive**: server (`internal/mite/server.go`) sets `KeepaliveEnforcementPolicy(MinTime: 15s)` / `KeepaliveParams(Time: 30s)`; client (`cmd/mite/agent.go`) pings every 30s. Mismatched policies make the server send `GOAWAY ENHANCE_YOUR_CALM`, silently killing the stream.
- **Send serialization**: `sendMu` (`agent.go:57`) serializes concurrent `stream.Send()` from the heartbeat goroutine and `processCommands`.
- **Context unblocking**: `processCommands` wraps `stream.Recv()` in a goroutine + channel so `ctx.Done()` breaks the loop; on Send error, `sendHeartbeats` calls `a.conn.Close()` to unblock the Recv.
- **Mite Jenkins auth**: Bearer JWT; the operator signs an RS256 JWT (`MiteTokenSigner` in `internal/mite/jenkinstoken.go`) and pushes it over gRPC. The mite caches it in-memory (`currentJenkinsToken()` in `cmd/mite/agent.go`). The Jenkins plugin verifies the JWT offline with the operator's public key (no Dex dependency).
- **Waiting on cluster state**: a short retry/backoff loop beats a fixed sleep for anything waiting on reconciliation to converge: `for i in $(seq 1 30); do <check> && break; sleep 5; done`.
- **Frontend commands**: always run `npm` commands from `frontend/`; don't assume the shell's cwd already points there.

## Linting

golangci-lint v2 (`revive`, `gofmt`, `goimports`). Local import prefix `github.com/varroaci/varroa-jenkins` goes **last** in import groups. Proto-generated code under `internal/mite/proto/mitev1/` is excluded.

The golangci-lint version is pinned, and `.github/workflows/pr.yaml`, `.github/workflows/release.yaml`, and `GOLANGCI_LINT_VERSION` in the `Makefile` (what `make lint` runs locally) must all pin the **same** version. `@latest` means a mid-week golangci-lint release turns every open PR red on untouched code; pinning only some of the three is worse still, because a PR passes lint on one workflow, merges, and then fails on the other after review is over, where the only symptom is that no image published. Bump all three in one deliberate PR, with the resulting findings fixed in the same change.

## CI

GitHub Actions (`.github/workflows/`), running on GitHub-hosted `ubuntu-latest` runners: `pr.yaml` runs lint, check-crds, check-proto, check-client, govulncheck, race tests, and docker builds on every PR; `pr-frontend.yaml` covers frontend-only changes; `release.yaml` publishes images and the Helm chart on tag pushes.

Never assume a CLI is preinstalled on the runner image without checking. Pull it in via `uses: ./.github/actions/setup-ci-tools` (inputs `yq`, `gh`, `make`, `helm`, `shellcheck`, `envsubst`; each no-ops when already on PATH) rather than adding another inline installer. There is no Jenkinsfile; CI runs on Actions.

Copilot code review reads `.github/copilot-instructions.md`, and also ingests `AGENTS.md` + `CLAUDE.md` as custom instructions. It loads all instruction files from the **base branch**, so edits only take effect once merged to `main`. When repo conventions change here, keep `copilot-instructions.md` aligned so review comments stay low-noise.

## Repository layout

- `api/`: CRD Go types (`v1alpha1`, group `varroa.dev/v1alpha1`) and the OpenAPI 3 contract.
- `assets/`: static images/icons used by the frontend and docs.
- `bundles/`: JCasC content embedded in the operator binary (the built-in starter bundle).
- `charts/varroa/`: the umbrella Helm chart (split backend + Dex + NATS), generated CRDs and version profiles.
- `cmd/`: binaries/entrypoints: `operator`, `gateway`, `bff`, `mite`, `varroactl`, `fakemite`, `protogen`.
- `config/`: reference/example Kubernetes manifests.
- `docs/`: the user-facing Varroa Operator Handbook.
- `examples/`: sample CRs (`Controller`, `CatalogSource`, …) to apply against a running cluster.
- `frontend/`: React/TypeScript/Vite dashboard.
- `hack/`: dev-tooling scripts: plugin-lock generation, localdev (kind), mock OIDC, OpenAPI bundling.
- `internal/`: Go backend packages: `controller`, `api`, `mite`, `bundle`, `rbac`, `bus`, and more.
- `legal/`: the commercial license agreement template (see `COMMERCIAL-LICENSE.md`).
- `pkg/`: the repo's public/consumable Go surface: generated OpenAPI client, stream helper, embedded templates.
- `plugin/`: the `VarroaSecurityRealm` Jenkins plugin (Maven/HPI, JDK 21).
- `scripts/`: small CI/dev helper scripts.
