# charts/varroa AGENTS.md

## Purpose

Umbrella Helm chart (`Chart.yaml`, `apiVersion: v2`, name `varroa`, `kubeVersion: ">= 1.30"`)
deploying the Varroa control plane: sharded `varroa-operator`, `varroa-gateway` (mite
gRPC :9090), `varroa-bff` (HTTP API/SSE :8080), `varroa-frontend`, `dex` (OIDC broker),
`oauth2-proxy`, NATS JetStream (subchart `nats` v1.2.0, condition `nats.enabled`), and
`prometheus`/`grafana`.

## Ownership

Owns chart shape: which components render, RBAC scoping, security-context defaults, and
the packaged CRDs/version-profile manifests. Does not own the Go source it packages
(`internal/`/`cmd/`, root AGENTS.md) — this tree only templates it.

## Local Contracts

- **`templates/<component>/`** — one dir per workload: `bff/`, `dex/`, `frontend/`,
  `gateway/`, `grafana/`, `ingress/`, `oauth2-proxy/`, `operator/`, `prometheus/`,
  `version-profiles/`, each with `deployment.yaml`/`service.yaml`/`serviceaccount.yaml` +
  RBAC where the component talks to the API server. `bff/` also owns
  `role-rbac-federation.yaml`/`rolebinding-rbac-federation.yaml` (JenkinsRole federation).
  The internet-facing BFF holds no cluster-wide Secret write and no cluster-wide
  ConfigMap read: its `configmaps` `get` (version-profile plugin sets, issue #416)
  lives on the release-namespace `Role` in `bff/role.yaml`, because
  `OPERATOR_NAMESPACE` is fixed to `.Release.Namespace`. Asserted by
  `tests/test-bff-configmap-rbac.sh`.
- **Root templates**: `_helpers.tpl` (`varroa.fullname`, `varroa.isHive`,
  `varroa.natsServerCount`, `varroa.jetStreamReplicas`);
  `mode-guard.yaml`/`oidc-guard.yaml` render nothing, only `fail` on invalid `mode`,
  missing hive/OIDC prereqs, or an explicit `jetStreamReplicas` exceeding the actual
  NATS server count; `networkpolicy.yaml` (all opt-in, `networkPolicy.enabled`);
  `pdb.yaml` (operator/gateway/bff PDBs, only when replicas > 1; bff PDB skipped in hive
  mode); `nats-auth-config.yaml`/`secret-nats-creds.yaml`/`nats-external-service.yaml`
  (NATS TLS + per-service ACL creds); `examples-item-rbac.yaml` (off by default,
  `examples.itemRbac.enabled`).
- KV watchers require `$JS.FC.KV_<bucket>.>` publish grants.
- **`nats.config.cluster.enabled`/`nats.config.cluster.replicas` are the ONLY real
  clustering knobs** for the vendored nats-1.2.0 subchart (its `stateful-set.yaml`
  hardcodes `replicas: 1` whenever `cluster.enabled` is false, ignoring `replicas`
  entirely). There used to be a decoy top-level `nats.replicas` (issue #433) that the
  subchart never read — it is gone. `jetStreamReplicas` (VARROA_JETSTREAM_REPLICAS)
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
  pull-through gating. The BFF deployment receives
  `VARROA_UPDATE_CENTER_IMPORT_TOKEN` from the same import-token Secret,
  because the BFF is the only component that authenticates a real user and so
  the only one that can attribute an upload.
  `updateCenter.seed.refs` defaults to the first-party `varroa-mcp-tools`
  addon pack on ghcr (that plugin is not on `updates.jenkins.io`, so an
  air-gapped install has no other route to it); an operator override REPLACES
  the list. An unreachable seed is non-fatal — `derivePhase` does not consume
  `SeedImported`. Asserted by `tests/test-updatecenter-seed-default.sh`. The
  `updateCenter.*` values tree and the three NetworkPolicy egress toggles
  (`pullThroughEgress`/`updateCenterRegistryEgress`/`ociRegistryEgress`) are a
  pinned contract shared with `internal/updatecenter`; netpol
  shape asserted by `tests/test-updatecenter-netpol.sh` (hive mode stays exactly
  4 policies).
- **`observability/dashboards/varroa-telemetry.json`** +
  **`observability/rules/varroa-alerts.yaml`** — Grafana dashboard + Prometheus rules,
  **not wired into any template** (grafana/prometheus configmaps only render
  `.Values.*.config` verbatim) — manually imported.
- **`templates/version-profiles/*.yaml`** — default `JenkinsVersionProfile` CRs
  (`2.555-profile.yaml`, `2.570-profile.yaml`) + owned `*-pluginset-configmap.yaml`,
  generated by `hack/gen-plugin-lock.sh` from `hack/version-profiles.yaml`. Never
  hand-edit — a stale file silently pins the wrong plugin set for that LTS line.
- **`tests/*.sh`** — golden-render bash assertions over `helm template` output
  (`test-hive-mode.sh`, `test-secret-rbac-scoping.sh`, `test-nats-bus-security.sh`); not
  the `helm unittest` plugin.
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

## Work Guidance

- A CRD field change in `api/v1alpha1/types.go` requires `make generate-crds` before
  touching this chart — never hand-patch `crds/*.yaml`.
- Plugin-lock/version changes go through `hack/version-profiles.yaml` +
  `hack/gen-plugin-lock.sh`, not direct edits under `templates/version-profiles/`.
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
bash charts/varroa/tests/test-grafana-admin-secret.sh
bash charts/varroa/tests/test-dex-grpc-disabled.sh
bash charts/varroa/tests/test-updatecenter-seed-default.sh
bash charts/varroa/tests/test-updatecenter-uploads-nil-guard.sh
bash charts/varroa/tests/test-bff-configmap-rbac.sh
```
