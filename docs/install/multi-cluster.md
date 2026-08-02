# Adding a member cluster

## Dashboard configuration context

The dashboard carries the active authoring cluster in the URL as
`?cluster=<cluster-name>` across Catalog Sources, Catalog Items, Composed Bundles,
Jenkins Roles, and Jenkins Role Bindings. Links, reloads, forms, previews, and
mutations use that same cluster, so bookmarked configuration URLs are safe to
share. Composer drafts are stored separately for each cluster.

If the URL omits an eligible cluster or names an unavailable one, the dashboard
selects the healthy active core cluster, then the first healthy active cluster,
and replaces only the `cluster` query parameter. Other query parameters are
preserved. When no healthy active cluster is accessible, configuration pages do
not issue cluster-scoped requests and display `No accessible clusters.`

## Topology

Varroa's multi-cluster architecture uses a core-and-hive NATS spine:

- **Core cluster** (full mode): Deploys the complete control plane — operator,
  gateway, BFF, frontend, NATS JetStream, Dex, Prometheus, Grafana. This is
  the default deployment mode.
- **Hive** (hive mode): Deploys only the operator and gateway, which
  connect to the core's external NATS endpoint. No local NATS, BFF, frontend,
  or observability stack.

Membership is signaled by a heartbeat entry in the `varroa_clusters` KV bucket.
Each cluster's operator leader writes its `ClusterInfo` JSON (`name`,
`operatorVersion`, `k8sVersion`, `controllerCount`, `connectedCount`,
`lastHeartbeat`) every 30 seconds with a 90-second TTL. **Presence == health**:
if the operator dies, its entry expires within 90 seconds (three missed
heartbeats).

## 1. Expose the core NATS

On the core cluster, enable external NATS exposure so hives can reach
the NATS bus.

### Fresh install (recommended)

```bash
helm upgrade --install varroa charts/varroa \
  --set nats.external.enabled=true \
  --set nats.external.host=nats.core.example.com \
  --set nats.external.serviceType=LoadBalancer \
  [--set nats.external.extraSANs="{alt1.example.com,alt2.example.com}"] \
  [--set nats.external.ipSANs="{1.2.3.4}"] \
  [--set nats.external.annotations.'service\.beta\.kubernetes\.io/aws-load-balancer-type'=nlb] \
  # ... other values
```

If your cluster does not support LoadBalancer services, use NodePort:

```bash
  --set nats.external.serviceType=NodePort \
  --set nats.external.nodePort=30422
```

### Existing install (cert rotation required)

Enabling `nats.external` on an existing installation does **not** regenerate the
TLS certificate automatically (the `_helpers.tpl` memoizes via `lookup`). You
must force regeneration:

```bash
kubectl -n varroa-system delete secret varroa-nats-creds
helm upgrade varroa charts/varroa \
  --set nats.external.enabled=true \
  --set nats.external.host=nats.core.example.com \
  # ... same values as initial install
kubectl -n varroa-system rollout restart deploy/varroa-nats
kubectl -n varroa-system rollout restart deploy/varroa-operator
kubectl -n varroa-system rollout restart deploy/varroa-gateway
kubectl -n varroa-system rollout restart deploy/varroa-bff
```

Then re-copy the pruned secret to every hive (step 2 below).

> **Best practice**: Expose NATS before joining any hives to avoid
> the cert rotation dance.

## 2. Copy credentials

Hives need the NATS credentials (`operator-password`, `gateway-password`,
and `ca.crt`) but must **never** receive `ca.key`, `tls.key`, or `bff-password`
(server/CA private material and unused creds stay on the core).

From the core cluster, run:

```bash
kubectl -n varroa-system get secret varroa-nats-creds -o json \
  | jq '{apiVersion, kind, metadata: {name: .metadata.name},
         data: {"operator-password": .data["operator-password"],
                "gateway-password":  .data["gateway-password"],
                "ca.crt":            .data["ca.crt"]}}' \
  | kubectl --context <hive-context> -n <hive-ns> apply -f -
```

This creates a `varroa-nats-creds` Secret with exactly the three required keys.

## 3. Install the hive

On the hive, install using the hive overlay:

```bash
helm install varroa charts/varroa \
  -f charts/varroa/values-hive.yaml \
  --set cluster.name=<hive-name> \
  --set bus.url=tls://nats.core.example.com:4222 \
  --set auth.oidc.clientSecret=<core-oidc-client-secret> \
  # ... other auth.* values matching the core
```

The hive mode:
- Renders only the operator and gateway deployments, services, RBAC, and
  service accounts.
- Requires `cluster.name` (a DNS-1123 label identifying this cluster) and
  `bus.url` (the core's external NATS endpoint).
- Disables NATS, BFF, frontend, Dex, oauth2-proxy, Prometheus, and Grafana.

The operator's `auth.oidc.clientSecret` is still `required` in OIDC mode —
hive Jenkins pods authenticate against the core's issuer.

### Hive provisioning prerequisites

Controllers provisioned on the hive pull images and read defaults
from **that** cluster, so each hive also needs:

1. **A registry pull secret in every controller namespace** (the operator
   stamps it onto Jenkins StatefulSets via `ProvisioningDefaults`):

   ```bash
   kubectl -n <core-ns> get secret <registry-secret> -o yaml \
     | kubectl --context <hive-context> -n <controller-ns> apply -f -
   ```

2. **A cluster-scoped `ProvisioningDefaults`** named `varroa-defaults`
   (`imagePullSecrets`, storage class, ingress defaults — mirror the core's or
   tailor per cluster):

   ```yaml
   apiVersion: varroa.dev/v1alpha1
   kind: ProvisioningDefaults
   metadata:
     name: varroa-defaults
   spec:
     imagePullSecrets:
       - <registry-secret>
   ```

3. **Git bundle credentials**, if bundles come from private repos: copy the
   SSH key Secret and wire `operator.env` (`GIT_SSH_COMMAND`) +
   `operator.extraVolumes`/`extraVolumeMounts` exactly as on the core.

Without these, controller creation preflights on the hive fail (or
pods sit in `Init:ErrImagePull`). Symptoms are visible in the `checks` array
returned by create/preflight.

### NetworkPolicy on hives

The chart supports opt-in NetworkPolicy in hive mode. Enable with
`--set networkPolicy.enabled=true` or in your values file. The hive-mode
set renders exactly four policies: default-deny (all pods), DNS egress, and
per-workload policies for the operator and gateway.

Pin `networkPolicy.coreNatsEgress.cidrs` to the core NATS endpoint IP(s); the
port is auto-derived from `bus.url` (override with `coreNatsEgress.ports` for
multi-server or IPv6 `bus.url` forms).

`networkPolicy.tenantNamespaceSelector` controls which tenant/controller
namespaces can reach the gateway on :9090 (gRPC) and :9092 (apikey verify),
with the same semantics as full mode (default `{}` = all namespaces).

`networkPolicy.metricsIngress` allows user-managed scrapers to reach
operator :8080/:9091 and gateway :{grpcPort+1} metrics endpoints in hive
mode only (full mode keeps the built-in prometheus-source rules).

See [Network policies](network-policies.md) for the full reference.

## 4. Verify

From a nats-box pod on the core cluster:

```bash
nats kv get varroa_clusters <hive-name>
```

The output should show the heartbeat JSON with `lastHeartbeat` ≤ 30 seconds old.

On the hive, check the operator leader's metrics:

```bash
kubectl -n <hive-ns> exec deploy/varroa-operator -- \
  curl -s http://localhost:9091/metrics | grep varroa_cluster_heartbeats_total
```

The counter should increment on the leader replica.

On the core BFF, check the cluster count gauge:

```bash
kubectl -n <core-ns> exec deploy/varroa-bff -- \
  curl -s http://localhost:8080/metrics | grep varroa_clusters_known
```

Expected value: number of member clusters (core + hives).

## 5. Failure / expiry semantics

- If a hive operator dies, its `varroa_clusters` entry expires within 90
  seconds (three missed heartbeats × 30 s interval).
- The BFF's `varroa_clusters_known` gauge drops accordingly.
- **TTL changes** are not retroactively applied by `EnsureKV`. To change the
  TTL, delete and recreate the bucket (all entries are lost and will be
  repopulated within 30 seconds by live operators).
- **Presence == health**: there is no explicit `healthy` field. Downstream
  consumers (C6) derive health from entry presence and staleness from
  `lastHeartbeat`.

## Limitations

- **Shared NATS users**: all clusters share the `operator` and `gateway` NATS
  user accounts (subject-scoped ACLs limit each user to its own subjects based
  on the cluster-qualified naming scheme).
- **Remote management** (listing/controlling controllers on hives
  from the core UI) arrives with changes C6, C7, and C8.
- **Pre-upgrade activity history**: activity events recorded before the
  multicluster-capable release use the old subject naming and are not shown
  in per-controller history; they age out on their own — see [History horizon
  after the multicluster upgrade](../operations/observability.md#concepts-the-activity-feed).

## 6. Draining and decommissioning a cluster

The drain verb orders a cluster out of service. It deletes every Controller CR
on the target cluster through the normal reconciler deletion path (StatefulSets,
Services, Ingresses are torn down).

> **Jenkins data is NOT migrated anywhere.** PVCs are local to the drained
> cluster and are left orphaned after drain completes. Manual cleanup:
> `kubectl get pvc -A` on the drained cluster lists the orphaned volumes.

### State machine

A cluster transitions through three lifecycle states, visible in the
`GET /api/v1/clusters` response (`state` field), the `/clusters` page
(State pill), and `varroactl get clusters` (STATE column):

| State | Meaning |
|---|---|
| `active` | Normal operation — controllers can be created and managed. |
| `draining` | Drain in progress — the operator is deleting every Controller CR. New creates are blocked (HTTP 409). |
| `drained` | All Controller CRs have been deleted. The cluster remains visible but idle until canceled or uninstalled. |

Transitions:

- `active` → `draining`: `POST /api/v1/clusters/{cluster}/drain` (admin-only,
  confirm-body required).
- `draining` → `drained`: Automatic — the operator's drain runner observes
  zero Controller CRs and flips the state.
- `draining` → `active`: `DELETE /api/v1/clusters/{cluster}/drain` (cancel;
  in-flight CR deletions are not undone).
- `drained` → `active`: Same cancel verb (rejoin).

### Drain a cluster

```bash
# Interactive (prompts for confirmation):
varroactl drain cluster dev-cluster

# Non-interactive:
varroactl drain cluster dev-cluster --yes
```

The confirmation dialog and `--yes` skip state that **all controllers will be
deleted** and Jenkins data will not be migrated.

### Cancel a drain

```bash
varroactl drain cluster dev-cluster --cancel
```

Cancel returns the cluster to `active`. Controllers whose deletion was already
in flight are **not restored** — they will continue deleting.

### Recreate a controller on another cluster

Because Jenkins data is not migrated, recreating a controller on another
cluster starts with a fresh Jenkins home:

```bash
varroactl get controller NS/NAME -o yaml --cluster old-cluster
varroactl create controller -f - --cluster new-cluster
```

### Known quirk: stale ConfigMap on reinstall

Drain state is stored in a ConfigMap `varroa-cluster-lifecycle` in the
operator namespace. If you uninstall the hive chart (which does **not**
delete the runtime-created ConfigMap) and reinstall into the same namespace,
the operator will rejoin as `drained` — creates are blocked. Fix:

```bash
varroactl drain cluster NAME --cancel
```

### Final decommission

After the cluster is drained and you no longer need it, uninstall the hive
chart:

```bash
helm uninstall varroa -n <hive-ns>
```

The cluster's `varroa_clusters` KV entry will TTL-expire within 90 seconds
(three missed heartbeats) and disappear from `GET /api/v1/clusters`.

## 7. Cross-cluster brood operations

Brood operations (restart, reprovision, reconcile, stop, start) can span
multiple clusters from the core BFF. The `POST /api/v1/brood-operations`
endpoint accepts a `clusters` field, and the list/detail endpoints return an
aggregated per-run view across clusters. See
[Brood operations — Cross-cluster runs](../operations/brood-operations.md#cross-cluster-runs)
for the full reference.

## 8. Authoring configuration on hives

ComposedBundles, catalog sources/items, provisioning defaults, Jenkins version
profiles, and Jenkins RBAC (`JenkinsRole`/`JenkinsRoleBinding`) live **on the
cluster that consumes them**.
The core BFF authors them remotely over the bus — it does not replicate or mirror
config between clusters. Every config read and write is addressed to a specific
cluster; there is no aggregated cross-cluster config list (authoring is not
monitoring).

### Cluster-scoped API paths

The old flat config paths are gone (no redirects). All config resources are now
addressed under `/clusters/{cluster}/`:

```
GET|POST   /api/v1/clusters/{cluster}/composedbundles[/{ns}[/{name}]]
POST       /api/v1/clusters/{cluster}/composedbundles/{ns}/preview
POST       /api/v1/clusters/{cluster}/composedbundles/validate
GET|POST   /api/v1/clusters/{cluster}/catalogsources[/{ns}[/{name}]]
POST       /api/v1/clusters/{cluster}/catalogsources/{ns}/{name}/sync
GET        /api/v1/clusters/{cluster}/catalogitems[/{ns}/{name}]
GET|PUT    /api/v1/clusters/{cluster}/provisioningdefaults/{name}
GET        /api/v1/clusters/{cluster}/provisioning/config
GET|POST   /api/v1/clusters/{cluster}/version-profiles
GET|PUT|DELETE /api/v1/clusters/{cluster}/version-profiles/{name}
GET|PUT|DELETE /api/v1/clusters/{cluster}/jenkinsroles/{name}
GET|PUT|DELETE /api/v1/clusters/{cluster}/jenkinsrolebindings/{name}
```

Reads against the core are served directly; reads and all writes against a hive
cluster are brokered to that cluster's operator over NATS. If the target cluster
is unreachable the BFF returns `502 {"error":"cluster <name> unreachable"}`.
Catalog item lists return summary metadata without rendered `status.content` on
both local-direct and remote-over-bus paths; catalog item detail returns the full
CR including content. This keeps large catalogs listable across clusters.

### UI and CLI

- **Dashboard**: the bundle, catalog, provisioning, version profile, and Jenkins-role authoring pages carry a
  cluster selector; pick the target cluster before creating or editing. The
  controller wizard's cluster picker already scopes the bundle it references.
- **varroactl**: every config noun accepts `--cluster` (composedbundles,
  catalogsources, catalogitems, provisioningdefaults, versionprofiles,
  jenkinsroles, jenkinsrolebindings, plus the
  `validate bundle` / `preview bundle` / `pause bundle` / `resume bundle` /
  `sync catalogsource` verbs). Resolution precedence is `--cluster` >
  the active context's `defaultCluster` > `core`. Config lists are single-cluster;
  there is no `--all-clusters` for them.

### Preview and validation run on the target operator

`preview` and `validate` for a ComposedBundle execute on the **target cluster's**
operator, so the composed output reflects that cluster's catalog items, git
credentials, and variables — not the core's. The core BFF no longer composes
bundles itself and no longer reads git-auth Secrets outside its own release
namespace.

### Identical catalogs across clusters (same-repo pattern)

There is no catalog replication. To give several clusters the *same* catalog,
point a `CatalogSource` on each cluster at the **same git repository** (and
revision). Each cluster syncs it independently and materializes identical
`CatalogItem`s locally. This keeps every cluster self-contained while giving you
one source of truth in git.

### VarroaRole authoring stays core-local

`VarroaRole` and `VarroaRoleBinding` govern core-side authorization and remain
core-only — they are **not** cluster-scoped and cannot be authored on a
hive. Their Jenkins-facing projection still reaches hive controllers: the
core BFF copies each bound `jenkinsRoleRef` or legacy `jenkinsPermissions` grant
to every non-core member cluster as ordinary `JenkinsRole`/`JenkinsRoleBinding`
CRs over the existing `operator.<cluster>.rbac.*` bus subjects.

Federated CRs are labeled `varroa.dev/federated-from`. The copied
`JenkinsRole` name and binding `spec.roleRef` stay unprefixed so the hive
generates the same Jenkins role name as the core (`varroa:<name>`); only the
federated binding object's metadata name starts with `varroa-fed-`. If a hive
already has a same-named hand-authored role or binding object without
the federation label, the core skips that object and logs a warning instead of
overwriting it.
