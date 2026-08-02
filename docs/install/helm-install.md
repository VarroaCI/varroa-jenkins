# Installing with Helm

<!-- sources: charts/varroa/Chart.yaml, charts/varroa/values.yaml, charts/varroa/crds/, charts/varroa/templates/, Makefile -->

Deploy the Varroa installation — control plane, dashboard, NATS, and the optional Dex/observability stack — with the umbrella chart at `charts/varroa`.

## Prerequisites

- Everything in [Prerequisites](prerequisites.md)
- A values file for your environment (built below)

## Concepts: what the chart deploys

One release installs: the **operator** (leader-elected reconcilers), **gateway** (mite gRPC on `:9090`), **BFF** (HTTP API/SSE on `:8080`), the **dashboard** frontend, a **NATS** cluster with JetStream (subchart, TLS + per-service credentials generated at install), the CRDs, and optionally **Dex**, **Prometheus**, **Grafana**, and **NetworkPolicies**. Default Jenkins version profiles ship as chart-managed `JenkinsVersionProfile` objects.

Values are grouped by area — the top-level keys you'll touch:

| Area | Keys | What it controls |
|---|---|---|
| Global | `global.*`, `managedNamespaces` | Domain, image defaults, which namespaces Varroa manages |
| Auth | `auth.*` (`mode`, `oidc.*`, `cookieDomain`, …) | How users log in — see [Authentication](../security/authentication.md) |
| Cluster | `cluster.name` | Cluster identity token used in all NATS subjects/keys; DNS-1123 label; default `core` |
| Dex | `dex.*` | Optional OIDC broker (GitHub OAuth, SAML, brokered LDAP) |
| Components | `operator.*`, `gateway.*`, `bff.*`, `frontend.*` | Images, replicas, resources per component |
| Bus | `nats.*`, `jetStreamReplicas` | NATS subchart (replicas, JetStream storage) and the stream/KV replica count — see [NATS JetStream replication](#nats-jetstream-replication) |
| Activity | `activity.*` | Activity feed retention — see [Observability](../operations/observability.md) |
| Observability | `prometheus.*`, `grafana.*`, `telemetry.*` | Bundled monitoring stack, OTLP export. Grafana's admin password is auto-generated into `<release>-grafana-admin` and printed in `helm install`/`upgrade` NOTES output. Retrieve with `kubectl get secret <release>-grafana-admin -o jsonpath='{.data.admin-password}' | base64 -d; echo`.
| Network | `networkPolicy.*` | Opt-in policies — see [Network policies](network-policies.md) |
| Update Center | `updateCenter.*` | In-cluster plugin metadata serving, storage backends, pull-through caching, seed import — see [Update Center](../operations/update-center.md) |
| Ingress | `ingress.*` | Dashboard ingress — see [Ingress](ingress.md) |

## Deployment modes

The chart supports two deployment modes controlled by the `mode` value:

### Full mode (default)

`mode: full` deploys the complete control plane: operator, gateway, BFF,
frontend, NATS JetStream, Dex (optional), Prometheus, Grafana, oauth2-proxy,
and NetworkPolicies. The core cluster runs in full mode.

### Hive mode

`mode: hive` deploys only the operator and gateway (and optionally NetworkPolicies), joined to a core cluster's external NATS endpoint. Requires `cluster.name` and `bus.url` (the core's
external NATS endpoint). Use with `charts/varroa/values-hive.yaml`:

```bash
helm install varroa charts/varroa \
  -f charts/varroa/values-hive.yaml \
  --set cluster.name=<hive-name> \
  --set bus.url=tls://nats.core.example.com:4222 \
  --set auth.oidc.clientSecret=<core-secret>
```

See [multi-cluster](multi-cluster.md) for the full hive-install runbook.

## How to install

1. Fetch chart dependencies (NATS is a subchart):

   ```bash
   helm dependency update charts/varroa
   ```

2. Create your values file. A minimal direct-OIDC install (the values below are illustrative — swap in your domain and issuer):

   ```yaml
   # values-prod.yaml
   global:
     domain: example.com
   auth:
     mode: oidc
     cookieDomain: .example.com        # leading dot: shares login across subdomains
     dashboardUrl: https://app.example.com  # required for OIDC; used for post-logout redirect
     oidc:
       issuer: https://login.example.com/
       clientId: varroa-dashboard
       clientSecret: "<client-secret>"
       redirectUrl: https://app.example.com/api/v1/callback
   dex:
     enabled: false                    # your IdP speaks OIDC natively
   ```

   > [!IMPORTANT]
   > `auth.cookieDomain` must be your parent domain with a leading dot. The login cookie must be valid on both the dashboard (`app.example.com`) and every controller host (`<name>.example.com`) for SSO into Jenkins to work.

3. Install:

   ```bash
   helm install varroa charts/varroa -n varroa --create-namespace -f values-prod.yaml
   ```

   > [!NOTE]
   > `helm template` without a cluster renders fresh random NATS credentials on every run (the chart uses `lookup` for credential stability). That's expected — only `helm install`/`helm upgrade` against a live cluster is a supported deploy path.

4. **Verify:**

   ```bash
   kubectl get pods -n varroa
   # operator (×3), gateway (×2), bff (×2), frontend, nats (×3) — all Running/Ready
   kubectl get crds | grep varroa.dev
   # 15 CRDs
   kubectl get jenkinsversionprofiles
   # the shipped default profiles
   ```

   Then open `https://app.<your-domain>` and log in.

## How to override images

Published images default to `ghcr.io/varroaci/varroa-jenkins` (backend, one image for operator/gateway/bff) and the frontend image. Point at your own registry or pin a build:

```yaml
operator: { image: { repository: registry.example.com/varroa-jenkins, tag: v0.3.0 } }
gateway:  { image: { repository: registry.example.com/varroa-jenkins, tag: v0.3.0 } }
bff:      { image: { repository: registry.example.com/varroa-jenkins, tag: v0.3.0 } }
frontend: { image: { repository: registry.example.com/varroa-frontend, tag: v0.3.0 } }
```

**Verify:** `kubectl get deploy -n varroa -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.template.spec.containers[0].image}{"\n"}{end}'` shows your registry/tags.

Building from source is a contributor workflow: `make docker-build` and `make frontend-docker-build` (see the `Makefile` and `CLAUDE.md`); this handbook assumes published or mirrored images.

## NATS JetStream replication

Every JetStream stream (activity history, webhook replay, imperative commands) and KV bucket (mite snapshots, presence, desired state, clusters, consumed tokens) Varroa creates is replicated to the value of `jetStreamReplicas`, rendered into the operator/BFF/gateway as `VARROA_JETSTREAM_REPLICAS`. This keeps that state available if a single NATS pod (and its PVC) is lost.

- **Default:** `jetStreamReplicas` is unset and derives from the nats-1.2.0 subchart's own clustering keys, `nats.config.cluster.enabled` / `nats.config.cluster.replicas`, **clamped to the range 1..3**. `nats.config.cluster.enabled: false` always renders exactly one standalone NATS server regardless of `replicas`, so it derives to 1; otherwise it derives from `nats.config.cluster.replicas`, floored at 1 (JetStream needs at least one replica) and capped at 3 (R>3 is rarely useful, so a NATS cluster larger than 3 still tops out at 3-way replication). Set `jetStreamReplicas` explicitly to override (it is also clamped 1..3). A chart-render guard fails the install if an explicit `jetStreamReplicas` exceeds the actual NATS server count.
- **A 3-node NATS cluster therefore replicates all soft state 3 ways by default.** A single-node NATS (e.g. localdev, which sets `nats.config.cluster.enabled: false`) yields R1 — a 1-node server cannot host R>1.
- **Hive mode** disables the local NATS subchart but still renders this env into the operator/gateway from `nats.config.cluster.replicas`, so they replicate against the **core's** NATS cluster. Set `jetStreamReplicas` (or `nats.config.cluster.replicas`) to match the core's cluster size if it differs from the default of 3.
- **Historical footgun (issue #433):** there used to be a decoy top-level `nats.replicas` key here — it looked authoritative but the subchart never read it, so a `nats.replicas: 3` install silently rendered as one standalone NATS server (no `cluster {}` stanza) while Varroa still requested R3 JetStream streams, which fail outright (`replicas > 1 not supported in non-clustered mode`). That key is gone; `nats.config.cluster.*` is the only knob and it drives both the subchart and `jetStreamReplicas` from the same source.

> [!NOTE]
> **Adopting R3 on an existing install.** Streams scale in place: the operator/BFF re-`EnsureStream` on every start, which falls back to `UpdateStream`, so existing durable streams move R1→R3 on the next reconcile (NATS 2.11). **KV buckets do not update their replica count in place** — `CreateKeyValue` only sets replicas at first creation. To adopt R3 for a pre-existing soft-state bucket, delete and recreate it (`nats kv del <bucket>`); the soft state (snapshots, presence, desired state) repopulates within ~30-90s from live mite heartbeats and operator reconciles, which is acceptable.

## How to upgrade

1. Apply CRD updates first — Helm installs CRDs from `charts/varroa/crds/` on first install but **does not upgrade them**:

   ```bash
   kubectl apply -f charts/varroa/crds/
   ```

2. Upgrade the release, reusing your values:

   ```bash
   helm upgrade varroa charts/varroa -n varroa -f values-prod.yaml
   ```

   > [!WARNING]
   > If you maintain values overrides outside your values file (e.g. `--set` flags from earlier operations), `helm upgrade -f` alone flattens them. Keep everything in the values file, or use `--reuse-values` deliberately.

3. **Verify:** `helm status varroa -n varroa` shows the new revision deployed; all pods roll and return to Ready; existing controllers stay `Connected` (mites reconnect across gateway restarts automatically).

Upgrading the control plane does **not** upgrade your Jenkins controllers — controller versions are governed separately; see [Jenkins versions](../config/jenkins-versions.md).

## How to uninstall

1. Delete all controllers first, so the operator can tear down their resources (StatefulSets, PVCs, ingresses) cleanly:

   ```bash
   kubectl delete controllers --all -A
   ```

2. Then remove the release and, if desired, the CRDs:

   ```bash
   helm uninstall varroa -n varroa
   kubectl delete -f charts/varroa/crds/   # optional: removes all Varroa CRs cluster-wide
   ```

**Verify:** `kubectl get pods -n varroa` is empty; no orphaned StatefulSets remain in controller namespaces.

## Troubleshooting

- Pods CrashLoopBackOff on first install → check NATS came up first (`kubectl logs deploy/varroa-varroa-bff -n varroa` for connection errors); JetStream needs its PVCs bound.
- Login redirect loop → `auth.oidc.redirectUrl` doesn't match the IdP's registered callback (use `/api/v1/callback`), or the dashboard host lacks valid TLS (Secure cookie dropped).
- More in [Troubleshooting](../operations/troubleshooting.md).

## Values reference

### Update Center (`updateCenter.*`)

| Key | Default | Description |
|---|---|---|
| `updateCenter.enabled` | `false` | Enable the in-cluster update-center component |
| `updateCenter.image.repository` | `ghcr.io/varroaci/varroa-jenkins` | Image repository |
| `updateCenter.image.tag` | `latest` | Image tag |
| `updateCenter.replicas` | `2` | Replicas (ignored when `storage.type=local`, which forces 1 + Recreate) |
| `updateCenter.storage.type` | `local` | Storage backend: `local` (PVC) or `oci` (OCI registry) |
| `updateCenter.storage.local.size` | `10Gi` | PVC size (used when `type=local`) |
| `updateCenter.storage.local.storageClassName` | `""` | PVC storage class (empty = cluster default) |
| `updateCenter.storage.oci.ref` | `""` | OCI image reference for the store content (required when `type=oci`) |
| `updateCenter.storage.oci.existingSecret` | `""` | Name of a `kubernetes.io/dockerconfigjson` Secret for registry auth |
| `updateCenter.storage.oci.insecure` | `false` | Allow HTTP (plaintext) connections to the registry |
| `updateCenter.pullThrough.enabled` | `false` | Enable pull-through caching from an upstream update center |
| `updateCenter.pullThrough.upstreamURL` | `https://updates.jenkins.io` | Upstream update-center JSON URL |
| `updateCenter.pullThrough.downloadURL` | `https://updates.jenkins.io/download` | Upstream HPI download base URL |
| `updateCenter.seed.refs` | `[]` | List of OCI refs to seed on startup |
| `updateCenter.importToken` | `""` | Stable token for `/api/v1/import`. Auto-generated and preserved across `helm upgrade` if empty; **set an explicit value for GitOps/ArgoCD** (client-side `helm template` has no cluster lookup, so auto-gen rotates every sync) |

### Network policy egress toggles (`networkPolicy.*`)

Three toggle groups control egress to external destinations for the update center and operator. Each follows the same shape:

```yaml
networkPolicy:
  <toggleName>:
    enabled: true
    cidrs: ["0.0.0.0/0"]
    ports: [443]
```

| Toggle | Component | When it renders | Purpose |
|---|---|---|---|
| `ociRegistryEgress` | operator | Always (mode-independent) | Operator reaches an external OCI registry for plugin-pack import and catalog operations |
| `pullThroughEgress` | updatecenter | Only when `updateCenter.pullThrough.enabled=true` | Update center reaches an upstream for pull-through caching |
| `updateCenterRegistryEgress` | updatecenter | Only when `updateCenter.storage.type=oci` | Update center reaches an external OCI registry for the seed store |

Pin the CIDRs to your registry addresses in air-gapped setups — see [Air-gapped installation](air-gapped.md).

## Related pages

- [Ingress](ingress.md) — expose the dashboard and controllers
- [Network policies](network-policies.md) — lock the installation down
- [Authentication](../security/authentication.md) — full auth-mode reference
- [Your first controller](../tutorials/first-controller.md) — what to do next
