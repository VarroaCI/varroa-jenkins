# charts/varroa AGENTS.md

## Purpose

Umbrella Helm chart (`Chart.yaml`, `apiVersion: v2`, name `varroa`, `kubeVersion: ">= 1.30"`)
deploying the Varroa control plane: sharded `varroa-operator`, `varroa-gateway` (mite
gRPC :9090), `varroa-bff` (HTTP API/SSE :8080), `varroa-frontend`, `dex` (OIDC broker), and
NATS JetStream (subchart `nats` v1.2.0, condition `nats.enabled`). The chart
ships no observability stack — Prometheus/Grafana are managed separately;
components expose metrics endpoints and `networkPolicy.metricsIngress` admits
external scrapers.

## Ownership

Owns chart shape: which components render, RBAC scoping, security-context defaults, and
the packaged CRDs. Does not own the Go source it packages
(`internal/`/`cmd/`, root AGENTS.md) — this tree only templates it.

## Local Contracts

- **`templates/<component>/`** — one dir per workload: `bff/`, `dex/`, `frontend/`,
  `gateway/`, `ingress/`, `operator/`, each with `deployment.yaml`/`service.yaml`/`serviceaccount.yaml` +
  RBAC where the component talks to the API server. `bff/` also owns
  `role-rbac-federation.yaml`/`rolebinding-rbac-federation.yaml` (JenkinsRole federation).
  `auth.localAccounts.enabled` renders `VARROA_LOCAL_ACCOUNTS` into the operator
  and the BFF; `templates/local-accounts-guard.yaml` fails the render unless
  `auth.mode` is `oidc` and `auth.bffUrl` is set
  (`tests/test-local-accounts.sh`).
  The internet-facing BFF holds no cluster-wide Secret write and no cluster-wide
  ConfigMap read: its `configmaps` `get` (version-profile plugin sets, issue #416)
  lives on the release-namespace `Role` in `bff/role.yaml`, because
  `OPERATOR_NAMESPACE` is fixed to `.Release.Namespace`. Asserted by
  `tests/test-bff-configmap-rbac.sh` (default mode) and
  `tests/test-secret-rbac-scoping.sh` (scoped mode).
  The BFF's **only** ConfigMap read outside the release namespace is a
  name-scoped grant the operator materializes per composed bundle: a `Role` +
  `RoleBinding` named `<bundle>-content-reader` in the bundle's own namespace,
  granting `get` on `status.contentRef` alone, owner-referenced to the bundle.
  The chart renders no part of it — the operator writes it against the live API
  server — but the chart does supply the subject through
  `VARROA_BFF_SERVICE_ACCOUNT` on the operator Deployment, rendered in core mode
  only (`tests/test-operator-bff-sa-env.sh`). Hive mode deploys no BFF, leaves
  the env unset, and gets no grant. Narrowing the operator's own `configmaps`
  `get` or `roles`/`rolebindings` write in a namespace breaks the grant: the
  Kubernetes escalation check refuses a `Role` the writer cannot itself hold.
- **Root templates**: `_helpers.tpl` (`varroa.fullname`, `varroa.isHive`,
  `varroa.natsServerCount`, `varroa.jetStreamReplicas`, `varroa.podPlacement` —
  renders `nodeSelector`/`tolerations`/`affinity` from a component's values
  scope, e.g. `{{- include "varroa.podPlacement" .Values.operator }}`; wired
  into operator/gateway/bff/dex deployments immediately
  before `containers:` (indentation is baked into the helper for that shared
  6-space nesting level); empty by default so a stock render is unaffected.
  `updateCenter.nodeSelector` predates this helper and stays separate. NATS is
  a subchart, not reachable through this helper — place it via
  `nats.podTemplate.merge.spec.nodeSelector`/`.tolerations`/`.affinity` and
  `nats.container.merge.resources` instead);
  `mode-guard.yaml`/`oidc-guard.yaml` render nothing, only `fail` on invalid `mode`,
  missing hive/OIDC prereqs, or an explicit `jetStreamReplicas` exceeding the actual
  NATS server count; `networkpolicy.yaml` (all opt-in, `networkPolicy.enabled`);
  `pdb.yaml` (operator/gateway/bff PDBs, only when replicas > 1; bff PDB skipped in hive
  mode); `nats-auth-config.yaml`/`secret-nats-creds.yaml`/`nats-external-service.yaml`
  (NATS TLS + per-service ACL creds); `examples-item-rbac.yaml` (off by default,
  `examples.itemRbac.enabled`).
- KV watchers require `$JS.FC.KV_<bucket>.>` publish grants.
- `nats-auth-config.yaml` grants no identity any `mite_desired` KV
  operation. The desired-state command carries credential values and travels
  only on `mite.*.*.*.desired` (operator publish, gateway subscribe).
  `tests/test-nats-bus-security.sh` asserts both rules.
- **`cells:`** (`values.yaml`) **seeds a `CellPool`; it configures no live
  pool.** Each of the five values populates a `CellPool` the operator creates
  for a requested profile, and the operator never rewrites the spec of a pool
  that already exists — so a Helm upgrade never rescales a running pool, and
  an operator's `varroactl scale cellpool` / `kubectl patch` edit survives it.
  `warmPool.enabled` gates **auto-creation only** (off by default; a
  hand-applied `CellPool` still reconciles), `warmPool.perProfile` seeds
  `spec.warm`, `maxTenantsPerCell` seeds `spec.maxTenantsPerCell`, and
  `unreadyGracePeriod` seeds `spec.unreadyGracePeriod` (a Go duration string),
  and `cellTemplate` seeds `spec.cellTemplate` through `VARROA_CELLS_CELL_TEMPLATE`
  (`toJson`, rendered `{}` for null). Helm merges a user map into the default
  map, so `{}` keeps the default: `cells.cellTemplate: null` is the only opt-out,
  and a single key is removed by setting it to `null`. The default must equal
  `defaultCellTemplate()` in `internal/controller/cells_config.go` (a Go test
  compares them); `tests/test-cells-operator-env.sh` asserts the render.
  The last two stay live as the fallback for a cell with no owning pool. The
  operator reads them through `ParseCellsConfigFromEnv`
  (`internal/controller/cells_config.go`), see
  [internal/controller/AGENTS.md](../../internal/controller/AGENTS.md).
  `templates/cells-guard.yaml` fails the render when `maxTenantsPerCell` is
  below 1 or `warmPool.perProfile` is below 1, mirroring the CellPool CRD floor
  and the operator seed floor (issue #628) instead of a silent fallback or a rejected
  apiserver write. It also refuses a value that is not a whole number or that
  exceeds int32. Both deployments render every cells value they seed through
  the `varroa.cells.int` helper it defines (the BFF renders
  `maxTenantsPerCell` only), so a values-file number never reaches the env
  in exponent form. Asserted by `tests/test-cells-guard.sh`.
  RBAC: the operator ClusterRole holds `cellpools` + `cellpools/status` with
  the full verb set (it creates, sizes, deletes, and holds the drain
  finalizer); the BFF holds `cellpools` with `get`/`list`/`watch`/`update`/
  `patch` and **neither `create` nor `delete`** — the API is a scale control,
  and a pool is created and deleted with kubectl or GitOps. Asserted by
  `tests/test-cellpool-rbac.sh`, which also asserts the packaged CRD and the
  four scalar `VARROA_CELLS_*` seed values.
  **`networkPolicy.cellIsolation`** (`enabled`, `extraIngress`,
  `extraEgress`) is the paired opt-in, honoured only when
  `networkPolicy.enabled` is also true. **The chart renders no per-cell
  NetworkPolicy template** — grep `templates/` and there is none. The
  operator builds and applies one `<prefix>-cell` NetworkPolicy per cell
  itself, every `Running`/`Connected` tick
  (`internal/controller/cell_networkpolicy.go`), because a cell's ingress
  and egress allow-list depends on live tenant placement
  (`Controller.status.cell.tenants`) that only the operator's own
  reconcile loop can see at render time. A tenant purge after a placement
  move needs two additional NATS grants alongside the existing
  `varroa.recordstore.*` set: the operator publishes
  `varroa.recordstore.tenant.purge` and subscribes `_INBOX_operator.>`
  (gated on `recordStore.enabled`); the recorder subscribes
  `varroa.recordstore.tenant.purge` and answers on `_INBOX_recorder.>`.
  Placement announcement and rehydration seeding need the matching pair for
  `varroa.recordstore.tenant.sync`: the operator publishes that subject
  alongside the tenant purge one, over the same `_INBOX_operator.>`
  subscribe grant; the recorder adds a second subscribe grant on
  `varroa.recordstore.tenant.sync` and answers through the
  `_INBOX_operator.>` publish grant it already holds for the tenant purge
  reply. Cross-tenant pipeline calls need one more request-reply pair,
  gated the same way: the gateway publishes `varroa.ledger.crosstenant`
  and the recorder subscribes it, queue group `recorder`, both only when
  `recordStore.enabled` — no other identity (operator, BFF, or a mite
  itself) holds a grant on this subject. The recorder publishes
  `varroa.ledger.actions.>`, never one cluster's subject: the one core
  recorder serves every member cluster, and a member renders no recorder.
  `tests/test-nats-bus-security.sh` asserts the wildcard and the absence of
  a single-cluster grant. Cell-loss detection adds one
  ungated grant: the operator **subscribes** `events.brood.>`, so a mite
  disconnect reaches a pod read at once. The operator already publishes
  `events.brood.*`, so its publish list is unchanged, and the recorder
  holds no brood grant in either direction — it decides no cell loss.
- **Startup probes on operator/gateway/bff are load-bearing, not boilerplate.**
  `bus.Connect` blocks for up to `bus.DefaultStartupTimeout` (3 minutes) waiting
  out a bus outage or a rotated Secret that has not synced, and no probe port
  listens until it returns. Each container therefore carries a `startupProbe` on
  its own liveness target with a 240s window (`periodSeconds: 10` ×
  `failureThreshold: 24`), which holds liveness and readiness off until it
  passes. **That window and `DefaultStartupTimeout` move together** — raising the
  Go budget without raising the probe window puts back the crash loop the retry
  exists to prevent. Asserted by `tests/test-nats-bus-security.sh`.
- **`varroa-nats-creds` is mounted whole-volume at `/etc/nats-creds`** on
  operator/gateway/bff, and the bus password reaches each of them as
  `-bus-pass-file=/etc/nats-creds/<component>-password`. Never a `subPath`
  mount, never an env var: kubelet refreshes only whole-volume Secret mounts, so
  either alternative freezes the credential at pod start and a rotation
  disconnects the component until someone restarts it. The operator's readiness
  probe hits `/readyz` (bus-aware) while liveness stays on `/healthz`. Asserted
  by `tests/test-nats-bus-security.sh`.
- **`nats.config.cluster.enabled`/`nats.config.cluster.replicas` are the ONLY real
  clustering knobs** for the vendored nats-1.2.0 subchart (its `stateful-set.yaml`
  hardcodes `replicas: 1` whenever `cluster.enabled` is false, ignoring `replicas`
  entirely). `jetStreamReplicas` (VARROA_JETSTREAM_REPLICAS)
  derives from these same two keys via `varroa.jetStreamReplicas`/`varroa.natsServerCount`,
  clamped 1..3, so the subchart's actual server count and Varroa's requested JetStream
  replication can no longer silently drift; `mode-guard.yaml` fails the render if an
  explicit `jetStreamReplicas` still exceeds the actual count.
- The operator wake surface is configured by `operator.wake.enabled/port` (defaults
  `true`/`8082`): Deployment env/port, EndpointSlice RBAC in both cluster-wide and
  managed-namespace modes, and an ingress-controller NetworkPolicy rule in full and
  hive modes must remain aligned.
- **`crds/`** — 19 generated `varroa.dev_*.yaml` CRD manifests (incl.
  `varroa.dev_updatecenters.yaml`). Generated from
  `api/v1alpha1/types.go` — never hand-edit. `make generate-crds` regenerates;
  `make check-crds` (CI `pr.yaml`) fails the build on drift.
  `varroa.dev_teams.yaml` carries `spec.pipelines.exports`
  (`TeamExport{Job, Locks, Callers}`, at most 64 entries), the tenant's
  outward cross-tenant grants, with its CEL rules bundled in.
- **`templates/updatecenter/`** — the opt-in update-center component (everything
  gated on `updateCenter.enabled` EXCEPT the CRD manifest, which always ships):
  deployment/svc/sa, `pvc.yaml` (local storage mode only; single replica,
  `Recreate`; carries `helm.sh/resource-policy: keep` so disabling
  `updateCenter.enabled` or uninstalling the release does NOT delete the
  plugin-store PVC — otherwise the cached blobs are lost), `secret.yaml`
  (`varroa-updatecenter-import-token`, idempotent),
  `updatecenter-cr.yaml` (the `varroa-update-center` singleton CR).
  `updateCenter.uploads.enabled` (default true) is the SINGLE source for both
  the single-writer topology (`replicas: 1` + `Recreate`, regardless of storage
  type) and `VARROA_UC_SINGLE_WRITER`, so the two cannot drift — replica count
  alone does not exclude a second writer, because a one-replica Deployment on
  the default `RollingUpdate` runs both pods during a rollout. Both
  dereferences of `updateCenter.uploads` (and `.maxBytes`) go through the
  nil-safe `$ucUploads`/`$ucUploadsEnabled` locals defined at the top of
  `deployment.yaml`, not a direct `.Values.updateCenter.uploads.enabled`
  chain: `helm upgrade --reuse-values` (or an explicit `--set
  updateCenter.uploads=null`) can leave that intermediate map nil, and a
  bare dotted chain panics instead of falling back to the documented
  default (issue #434). Do not swap this for a plain `| default true` on
  the bool — sprig's `default` treats `false` as empty and would silently
  re-enable uploads that were explicitly turned off. Asserted by
  `tests/test-updatecenter-uploads-nil-guard.sh`. The
  `varroa-updatecenter-metadata` ConfigMap is mounted UNCONDITIONALLY (gated
  only on `updateCenter.enabled`): its `declared-plugins` key is needed most
  when pull-through is off, while `VARROA_UC_LTS_METADATA_FILE` keeps its
  pull-through gating. `VARROA_UC_ARCHIVE_BASE_URL` is rendered behind a
  `hasKey` guard, NOT unconditionally: it distinguishes absent (server keeps its
  Maven-repository default) from empty (fallback disabled), so always emitting
  it would disable the fallback on a `--reuse-values` upgrade from a release
  predating `updateCenter.pullThrough.archiveURL`. That absent case is not
  reproducible under `helm template` — coalescing restores the default — so
  `tests/test-updatecenter-archive-url.sh` asserts only the three expressible
  states and the Go side covers unset. Note that this fallback targets
  `repo.jenkins-ci.org`, a DIFFERENT host from `pullThrough.upstreamURL`: a
  narrowed `networkPolicy.pullThroughEgress.cidrs` must cover it or the
  fallback is blocked. The BFF deployment receives
  `VARROA_UPDATE_CENTER_IMPORT_TOKEN` from the same import-token Secret,
  because the BFF is the only component that authenticates a real user and so
  the only one that can attribute an upload.
  `updateCenter.seed.refs` defaults to an empty list — seeding is opt-in — and
  an operator override REPLACES the list. `updateCenter.seed.secretRef` names a
  Secret in the OPERATOR namespace with pull credentials for those refs; only
  one registry's credentials are used, so all refs must share a registry. The
  `seed:` block in `updatecenter-cr.yaml` must render when EITHER `refs` or
  `secretRef` is set: guarding it on `refs` alone yields a valid CR that
  silently omits `secretRef`, and seeding stays anonymous with no error
  anywhere. An unreachable seed is non-fatal — `derivePhase` does not consume
  `SeedImported`. Asserted by `tests/test-updatecenter-seed-default.sh`, which
  also pins the storage/uploads replica topology, the disabled-render surface,
  and the pull-through defaults. The
  `updateCenter.*` values tree and the three NetworkPolicy egress toggles
  (`pullThroughEgress`/`updateCenterRegistryEgress`/`ociRegistryEgress`) are a
  pinned contract shared with `internal/updatecenter`; netpol
  shape asserted by `tests/test-updatecenter-netpol.sh` (hive mode stays exactly
  4 policies).
- **`templates/recorder/`** — the opt-in build record store component
  (`recordStore.enabled`): `deployment.yaml`/`service.yaml`/`serviceaccount.yaml`
  for the `varroa-recorder` workload, plus `postgres-cluster.yaml` for the
  `cnpg` Postgres mode. The recorder needs no Role or RoleBinding, because it
  never calls the Kubernetes API. See `internal/recordstore/AGENTS.md` and
  `docs/operations/build-records.md` for the values tree and the retained-data
  removal procedure. `tests/test-recorder-netpol.sh` asserts the
  `varroa-np-recorder` NetworkPolicy block; `tests/test-recorder-chart.sh`
  asserts the recorder workload shape, the CNPG `Cluster` and role Secrets,
  the DSN cross-references, the S3 credential mapping, the retention env
  defaults (`runs`/`logs`/`artifacts`/`activity`),
  `recordStore.cellLoss.confirmedGoneGrace` (default `30s`, rendered into
  `VARROA_RECORDSTORE_CONFIRMED_GONE_GRACE` as a **Go duration string**,
  not a millisecond count), the recorder PDB, and every
  `recordstore-guard.yaml` failure mode, including the retired `activity`
  values block. `recordStore.retention.activity` (default `30d`) replaced the
  old `activity.retention` dial: the recorder gets `VARROA_RETENTION_ACTIVITY`,
  the BFF gets `VARROA_ACTIVITY_RETENTION_DAYS` (both gated on
  `recordStore.enabled`), and `nats-auth-config.yaml` moved the
  `varroa_activity` JetStream grants from the BFF to the recorder — the BFF
  keeps only the core `activity.>` publish/subscribe grant.
  `tests/test-activity-stream-acl.sh` asserts the moved grants.
  The recorder Deployment carries the full `OTEL_*` env block
  (`OTEL_EXPORTER_OTLP_ENDPOINT`/`OTEL_TRACES_ENABLED`/`OTEL_METRICS_ENABLED`/
  `OTEL_LOGS_ENABLED`/`OTEL_LOG_LEVEL`/`OTEL_RESOURCE_ATTRIBUTES`), gated by
  its own `telemetry.services.recorder` values entry — the same shape as
  operator/gateway/bff. Without `OTEL_METRICS_ENABLED=true`,
  `internal/telemetry.InitTelemetry` never wires the Prometheus bridge, so
  every `otel.Meter("varroa-recorder")` counter in `internal/recordstore`
  stays off `/metrics` (issue #666). `tests/test-recorder-chart.sh`
  section (f2) asserts the env block and the
  `telemetry.services.recorder.metrics.enabled` toggle.
  In `cnpg` mode `postgres-cluster.yaml`'s `spec.managed.roles` carries three
  roles, each with its own `varroa-recordstore-<role>` Secret from
  `postgres-secrets.yaml`: `varroa_recorder` (the database owner),
  `varroa_client` (read plus `INSERT` on `log_chunks`, the BFF), and
  `varroa_gateway` (the same read-plus-log-chunks-insert shape, the gateway,
  only reached in `recordStore.blobs.mode: postgres`). `external` mode
  creates none of the three; every component shares the one `existingSecret`
  DSN instead. `recordStore.blobs.s3.artifactSecret` names the **base**
  artifact-credential Secret (`internal/controller/artifactcasc.go` reads
  `<artifactSecret>-<controller>` first, then `<artifactSecret>-<pool>` for a
  cell that carries a `varroa.dev/cell-pool` label, and projects the winning
  Secret into each controller's own namespace; this chart mounts nothing
  itself). The operator
  Deployment carries `VARROA_ARTIFACT_SECRET` (that base name, only when
  non-empty) and `VARROA_BLOBSTORE_URL` set to the identical value the
  gateway gets, both gated the same way as the gateway's own recordStore env
  block (`recordStore.enabled`, in both modes).
  **Hive mode joins the core record store directly.** With
  `recordStore.enabled` a hive render gives the gateway the blob URL and s3
  credential env (the DSN only in postgres blob mode) and the operator the
  blob URL, artifact Secret base, and claim gate, and renders no recorder,
  CNPG `Cluster`, role Secret, recorder PDB, or retention env.
  `recordstore-guard.yaml` runs in both modes: a hive must set
  `postgres.mode: external` and needs `existingSecret`/`dsnKey` only in
  postgres blob mode. The gateway NetworkPolicy carries the record-store
  egress rules in hive mode too; the hive set stays at four policies.
  `recordStore.postgres.memberEndpoint` (core, cnpg only, ignored in hive
  mode like `nats.external`) renders `<release>-recordstore-member` for the
  CNPG primary and a `np-recordstore-db` ingress rule from `allowedCIDRs`,
  which the guard requires when `networkPolicy.enabled` (an empty list
  would admit every source; a NodePort path masquerades the source, so the
  list must hold the addresses the traffic arrives from). `tests/test-hive-recordstore.sh`
  asserts all of it.
- **`tests/*.sh`** — golden-render bash assertions over `helm template` output
  (`test-hive-mode.sh`, `test-secret-rbac-scoping.sh`, `test-nats-bus-security.sh`,
  `test-recorder-netpol.sh`, `test-recorder-chart.sh`, `test-activity-stream-acl.sh`,
  `test-runner-door-nginx.sh`, `test-cells-guard.sh`, `test-hive-recordstore.sh`); not the
  `helm unittest` plugin.
- **The agent WebSocket door needs one nginx block, no Ingress change.**
  `frontend/nginx.conf`'s `location /runners/` block proxies to the BFF
  (`VARROA_API_HOST:8080`) and forwards the WebSocket upgrade: `Upgrade`,
  `Connection: upgrade`, `proxy_http_version 1.1`, and a 3600s read/send
  timeout. It cannot share the `/api/` block, which sets an empty
  `Connection` header and so suppresses the upgrade, or the `/hibernation/`
  block, which sets neither. The chart's Ingress already routes the whole
  host prefix `/` to the frontend Service, so no Ingress template changes.
  `tests/test-runner-door-nginx.sh` asserts the nginx block's upgrade
  contract, that `/api/` and `/hibernation/` stay unchanged, that
  `nginx -t` accepts the rendered config (via `envsubst` + `nginx:alpine`,
  since `nginx.conf` is a Docker `templates/` file, not a standalone
  `conf.d` file), and that the rendered Ingress carries no `/runners/` path.
- **`templates/frontend/configmap-telemetry.yaml`** — the static `window.__VARROA_TELEMETRY__`
  script the frontend reads at load. `tracesEnabled` is derived, not a direct passthrough of
  `telemetry.services.frontend.traces.enabled`: it also requires `telemetry.endpoint` be
  non-empty, since the BFF only registers `/api/v1/otel/v1/traces`
  (`OTEL_EXPORTER_OTLP_ENDPOINT`, `bff/deployment.yaml`) when that same value is set. Keeps
  one source of truth so an install that never configured a collector can't advertise traces
  as enabled and spam 404s on every page load (issue #437).
- **Hardening posture** (`values.yaml`): `networkPolicy.enabled` defaults `false`;
  `managedNamespaces: []` keeps operator/BFF ClusterRoles cluster-wide — an explicit list
  drops to per-namespace Roles (see `values.yaml:10-27`, `TargetNamespaceUnmanaged`
  footgun); `operator`/`gateway`/`bff` ship hardened `podSecurityContext`/
  `containerSecurityContext` (non-root uid 1000, `drop: [ALL]`,
  `readOnlyRootFilesystem: true`); `frontend` runs uid/gid 101. `mode: full|hive`
  (hive = operator+gateway only, joins an external core's NATS via `values-hive.yaml`);
  `values-localdev.yaml` is the `make localdev` kind overlay.
- **`values-scaletest.yaml`** — the EKS Auto Mode scale-test overlay. Private
  only: excluded from the public export alongside the scale-test tooling it
  belongs to, so it is absent from the public chart. Pins
  operator/gateway/bff/dex/NATS to a dedicated Karpenter NodePool via
  `varroa.podPlacement`/`nats.podTemplate.merge`, raises gateway/bff HPA
  bounds and operator pre-size, enables the update center, and turns on the
  dashboard ALB ingress. Deliberately account-value-free — the ACM
  certificate ARN and the cost-tracking tag ship as empty annotation
  placeholders that the session driver fills via `helm --set-string` at
  install time.

## Work Guidance

- A CRD field change in `api/v1alpha1/types.go` requires `make generate-crds` before
  touching this chart — never hand-patch `crds/*.yaml`.
- New workload components follow the per-component dir convention and must add hardened
  security-context blocks plus a `networkpolicy.yaml` peer entry.

## Verification

```bash
helm lint charts/varroa
helm template charts/varroa
bash charts/varroa/tests/test-hive-mode.sh
bash charts/varroa/tests/test-secret-rbac-scoping.sh
bash charts/varroa/tests/test-nats-bus-security.sh
bash charts/varroa/tests/test-mcp-netpol.sh
bash charts/varroa/tests/test-dex-grpc-disabled.sh
bash charts/varroa/tests/test-updatecenter-seed-default.sh
bash charts/varroa/tests/test-cells-guard.sh
bash charts/varroa/tests/test-cells-operator-env.sh
bash charts/varroa/tests/test-updatecenter-uploads-nil-guard.sh
bash charts/varroa/tests/test-updatecenter-archive-url.sh
bash charts/varroa/tests/test-bff-configmap-rbac.sh
bash charts/varroa/tests/test-operator-bff-sa-env.sh
bash charts/varroa/tests/test-recorder-netpol.sh
bash charts/varroa/tests/test-recorder-chart.sh
bash charts/varroa/tests/test-runner-door-nginx.sh
bash charts/varroa/tests/test-cellpool-rbac.sh
bash charts/varroa/tests/test-hive-recordstore.sh
```

- **Record-store Postgres NetworkPolicy** — in `recordStore.postgres.mode: cnpg` the chart renders
  `<release>-np-recordstore-db` for the CloudNativePG instance pods (`cnpg.io/cluster:
  varroa-recordstore`): ingress 5432 from the recorder, the BFF, the gateway, and the other
  instance pods; ingress 8000 from the operator namespace
  `networkPolicy.recordStoreCNPGOperatorNamespaceSelector`; egress to the API server and to the
  other instance pods. Without it the default-deny blocks initdb. `test-recorder-netpol.sh`
  asserts it.
