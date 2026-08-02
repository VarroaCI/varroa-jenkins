# Network Policies

<!-- sources: charts/varroa/templates/networkpolicy.yaml, charts/varroa/values.yaml (networkPolicy.*) -->

The chart ships an opt-in, default-deny NetworkPolicy set for the Varroa installation namespace, plus a selector controlling which tenant namespaces may reach the gateway. Off by default; turn it on once the installation works.

## Prerequisites

- A CNI that enforces `NetworkPolicy` (Calico, Cilium, …). With a non-enforcing CNI the policies apply silently and do nothing.
- A working install ([Helm install](helm-install.md)) — enable policies second, so you're debugging one thing at a time.

## Concepts

Enabling `networkPolicy.enabled=true` renders, in the release namespace:

1. **Default-deny** for all pods, plus a cluster-wide **DNS egress** allowance.
2. **Per-component policies** — operator, gateway, bff, frontend, dex, nats, prometheus, grafana, oauth2-proxy — each allowing only that component's required flows.

The flows that matter when things break:

| From | To | Port | Why |
|---|---|---|---|
| mite (controller namespaces) | gateway | 9090/TCP | gRPC mTLS command stream |
| controller namespaces | gateway | 9092/TCP | API-key verification |
| operator / gateway / bff | NATS | 4222/TCP | bus |
| operator / gateway / bff / dex / prometheus | kube-apiserver | 443, 6443/TCP | API access (`apiServerEgress`) |
| ingress controller namespace | frontend, bff | HTTP | dashboard + API |
| ingress controller namespace | operator | `operator.wake.port` (8082/TCP by default) | hibernation navigation-wake interstitial |
| operator | target Jenkins pods | 8080/TCP | [executeGroovy](../operations/brood-operations.md#executegroovy-semantics) dispatch — bimodal by `managedNamespaces` |
| operator / bff | updatecenter | 8080/TCP | seed import, inventory proxy — [see below](#update-center-flows) |
| bff / dex | identity provider | 443/TCP (+636 for LDAP) | OIDC/LDAP (`dexConnectorEgress`) |
| components | OTLP collector | 4317, 4318/TCP | only when `telemetry.endpoint` set |
| — | Dex gRPC admin API | 5557/TCP | **Disabled by default** (`dex.grpcApi.enabled=false`); even when enabled, the `np-dex` policy does **not** allow-list this port — add your own NetworkPolicy when needed |
| updatecenter | external OCI / upstream | 443/TCP | pull-through cache (`pullThroughEgress`) or OCI registry store (`updateCenterRegistryEgress`) — see [air-gapped](air-gapped.md) |

## How to enable

```yaml
networkPolicy:
  enabled: true
  # Match your ingress controller's namespace (labels, not name, if you've relabeled):
  ingressControllerNamespaceSelector:
    kubernetes.io/metadata.name: ingress-nginx
  # Which namespaces may reach the gateway (mite gRPC + API-key verify).
  # {} allows all namespaces; pin it to your tenant namespaces in production:
  tenantNamespaceSelector:
    varroa.dev/tenant: "true"
  # Pin apiserver egress to your control-plane endpoints instead of 0.0.0.0/0:
  apiServerEgress:
    cidrs: ["10.0.0.2/32"]
    ports: [443, 6443]
```

```bash
helm upgrade varroa charts/varroa -n varroa -f values-prod.yaml
```

**Verify:**

```bash
kubectl get networkpolicy -n varroa
# varroa-np-default-deny, varroa-np-dns, varroa-np-operator, varroa-np-gateway,
# varroa-np-bff, varroa-np-frontend, varroa-np-dex, varroa-np-nats, ...
kubectl get controller -A -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
# every controller still Connected — the mite→gateway flow survived
```

Then confirm enforcement by testing a flow that should now be blocked:

```bash
kubectl run np-test -n default --rm -it --image=busybox --restart=Never -- \
  nc -w 2 -zv varroa-varroa-nats.varroa.svc.cluster.local 4222 || echo BLOCKED
# BLOCKED  ← default namespace isn't a NATS client
```

## How to scope tenant access

With multi-tenant namespaces ([Multi-tenancy](../operations/multi-tenancy.md)), label each tenant namespace and set `tenantNamespaceSelector` to match. Only labeled namespaces can reach the gateway; a workload in any other namespace cannot even attempt mite registration or API-key verification.

```bash
kubectl label namespace teams-platform varroa.dev/tenant=true
```

**Verify:** a controller in a labeled namespace reaches `Connected`; a pod in an unlabeled namespace gets connection timeouts to `varroa-varroa-gateway:9090`.

## Troubleshooting

- Controllers drop to disconnected right after enabling → `tenantNamespaceSelector` doesn't match the controller namespaces; check namespace labels.
- Dashboard unreachable → `ingressControllerNamespaceSelector` doesn't match your ingress controller's namespace labels.
- Hibernated controller URL returns the ingress controller's raw 503 → the same selector does not allow ingress to the operator wake port, or `operator.wake.enabled=false`.
- Bundle composition fails with git timeouts → `gitEgress` disabled or your git host uses a non-standard port.
- OIDC login fails → `dexConnectorEgress` blocked, or the issuer is on a port other than 443.

### ExecuteGroovy egress

The [executeGroovy](../operations/brood-operations.md#executegroovy-semantics) verb dispatches Groovy scripts directly from the operator pod to the target controller's Jenkins on **TCP/8080** (bypassing the mite). This egress rule is added to the operator's NetworkPolicy (`<release>-np-operator`) and follows the same **bimodal `managedNamespaces`** convention used by the operator's RBAC scoping:

- **`managedNamespaces` empty** (default): the rule targets **all namespaces** (`namespaceSelector: {}`), since the operator may be asked to execute a script against any controller cluster-wide.
- **`managedNamespaces` populated** (e.g. `[teams-payments, teams-web]`): the rule renders one `namespaceSelector` per listed namespace, matching `kubernetes.io/metadata.name: <ns>`. Controllers in namespaces not listed here cannot be reached by `executeGroovy` when network policies are enforced.

No chart-managed **ingress** rule on the Jenkins side permits this traffic — the target controller's namespace may have its own default-deny policy that blocks operator→pod traffic. In that case the operator's egress succeeds but the target's ingress drops the packet. `ScriptConsoleOnce` surfaces this as a **transport error** (connection refused/timeout), bounded by the 60-second per-call deadline; the operator records it and marks the target `Failed` on the next poll tick. This is a fast, recorded failure — it does **not** wait for the 5-minute verb timeout (that timeout is a crash-backstop for when no result is ever written, not the path a blocked connection takes).

## Update Center flows

When `updateCenter.enabled=true` and `networkPolicy.enabled=true`, the following additional flows are rendered.

### Ingress to updatecenter

The `varroa-np-updatecenter` policy admits ingress on TCP 8080 from exactly three sources:

| Source | Selector | Why |
|---|---|---|
| Jenkins controller pods | `app.kubernetes.io/managed-by: varroa-operator`, bimodal by `managedNamespaces` | The update center serves plugin metadata and HPI downloads to controllers, replacing `updates.jenkins.io` |
| operator | `app.kubernetes.io/component: varroa-operator` | Seed import and inventory calls |
| bff | `app.kubernetes.io/component: varroa-bff` | Status/inventory proxy calls from the dashboard |

### Egress from updatecenter

The update center itself may need egress to two external destinations, each controlled by its own toggle:

| Toggle | Default | Gate | Destination |
|---|---|---|---|
| `pullThroughEgress` | `enabled: true` | also requires `updateCenter.pullThrough.enabled` | External upstream (`updates.jenkins.io` or an internal mirror) on TCP 443 |
| `updateCenterRegistryEgress` | `enabled: true` | also requires `updateCenter.storage.type=oci` | External OCI registry on TCP 443 |

### Egress from the operator: `ociRegistryEgress`

The operator's policy gains an additional mode-independent egress rule:

```yaml
# ociRegistryEgress: operator reaches external OCI registry (mode-independent)
- to:
    - ipBlock:
        cidr: 0.0.0.0/0
  ports:
    - protocol: TCP
      port: 443
```

This rule renders in **both full and hive mode** when `networkPolicy.ociRegistryEgress.enabled=true`. It allows the operator to pull plugin packs and catalog data from an external OCI registry (used by the [air-gapped install runbook](air-gapped.md)).

### Egress from the BFF to updatecenter

The BFF policy gains an egress rule to `varroa-updatecenter` pods on TCP 8080, gated on `updateCenter.enabled` (the BFF policy block already renders only in full mode). This allows the dashboard's status/inventory proxy to reach the update center.

### Full mode flows summary

| Source | Destination | Port | Gate | Mode |
|---|---|---|---|---|
| updatecenter | `pullThroughEgress` CIDRs | 443/TCP | `updateCenter.pullThrough.enabled` + `pullThroughEgress.enabled` | full |
| updatecenter | `updateCenterRegistryEgress` CIDRs | 443/TCP | `storage.type=oci` + `updateCenterRegistryEgress.enabled` | full |
| operator | updatecenter | 8080/TCP | `updateCenter.enabled` | full |
| operator | `ociRegistryEgress` CIDRs | 443/TCP | `ociRegistryEgress.enabled` | both |
| bff | updatecenter | 8080/TCP | `updateCenter.enabled` | full |
| Jenkins pods | updatecenter | 8080/TCP | `updateCenter.enabled` | full |

### Hive mode

In hive mode the `varroa-np-updatecenter` object never renders. The operator's dedicated egress rule to `varroa-updatecenter` pods is also absent. The operator's `ociRegistryEgress` rule **is** present (it is mode-independent and gated only on its own toggle, not on `updateCenter.enabled`). Hive mode's total NetworkPolicy count stays exactly four:

```
release-name-np-default-deny, release-name-np-dns,
release-name-np-operator, release-name-np-gateway
```

## Hive mode

Hive mode (`mode=hive`) deploys only the operator and gateway, connected to a
core cluster's external NATS endpoint. The NetworkPolicy set covers the same two
workloads — no BFF, frontend, Dex, NATS, or observability policies are rendered.

### Flows

| Source | Destination | Port | Why |
|---|---|---|---|
| operator / gateway | kube-apiserver | 443, 6443 | API access (`apiServerEgress`) |
| operator / gateway | core NATS endpoint | `bus.url` port (`coreNatsEgress`) | bus connection to core |
| operator | git hosts | 443, 22 | bundle clones (`gitEgress`) |
| operator | OCI registry | 443/TCP | plugin-pack import (`ociRegistryEgress`, mode-independent) |
| operator | target Jenkins pods | 8080/TCP | [executeGroovy](../operations/brood-operations.md#executegroovy-semantics) dispatch |
| ingress controller namespace | operator | `operator.wake.port` (8082/TCP by default) | hibernation navigation-wake interstitial |
| operator / gateway | OTLP collector | 4317, 4318 | only when `telemetry.endpoint` set |
| controller namespaces | gateway | 9090/TCP (gRPC) + 9092/TCP (apikey verify) | mite command stream + auth (`tenantNamespaceSelector`) |

### Enable

```yaml
networkPolicy:
  enabled: true
  coreNatsEgress:
    cidrs: ["<core-nats-endpoint-ip>/32"]   # pin to the core NATS endpoint
  tenantNamespaceSelector:
    varroa.dev/tenant: "true"
  apiServerEgress:
    cidrs: ["10.0.0.2/32"]
```

The core-NATS port is auto-derived from `bus.url`; set
`networkPolicy.coreNatsEgress.ports` explicitly for multi-server or IPv6 URLs.

### Verify

```bash
kubectl get networkpolicy -n varroa
# varroa-np-default-deny, varroa-np-dns, varroa-np-operator, varroa-np-gateway
kubectl get controller -A -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
# every controller still Connected
```

On the core, confirm the cluster heartbeat stays fresh:

```bash
nats kv get varroa_clusters <hive-name>
# lastHeartbeat should be ≤ 30s old
```

## Related pages

- [Helm install](helm-install.md) — where these values live
- [Multi-tenancy](../operations/multi-tenancy.md) — the tenant model this isolates
- [The mite](../architecture/mite.md) — the gateway flow being protected
- [Air-gapped installation](air-gapped.md) — deny-all-egress runbook and example manifests
