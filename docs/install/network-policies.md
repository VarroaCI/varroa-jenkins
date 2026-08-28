# Configure Network Policies

The chart can apply default-deny ingress and egress policies to the Varroa release namespace. The feature is disabled by default and requires a CNI that enforces Kubernetes `NetworkPolicy`.

The chart does not isolate controller namespaces. Apply separate policies there for Jenkins and agent workloads.

## Inventory Required Traffic

| Source | Destination | Port | Purpose |
|---|---|---:|---|
| Controller namespaces | Gateway | 9090, 9092/TCP | mite and key checks |
| Control plane | NATS | 4222/TCP | Bus |
| Control plane | Kubernetes API | 443, 6443/TCP | Resources |
| Ingress controller | Dashboard, BFF | 8080/TCP | HTTP |
| Ingress controller | Operator | 8082/TCP | Wake |
| Operator, BFF | Jenkins | 8080/TCP | Operations |

External egress depends on enabled features:

| Source | Destination | Ports | Purpose |
|---|---|---:|---|
| Operator, BFF | Git | 443, 22/TCP | Bundles |
| Operator | OCI registry | 443/TCP | Artifacts |
| Dex or BFF | Identity provider | Provider ports | Login |
| Components | OTLP collector | 4317, 4318/TCP | Telemetry |
| Jenkins, control plane | Update center | 8080/TCP | Plugins |

Cluster DNS is also required. Add any build-time destinations used by Jenkins or agents to tenant policies.

## Enable the Chart Policies

Start with selectors and API server addresses for your cluster:

```yaml
networkPolicy:
  enabled: true
  ingressControllerNamespaceSelector:
    kubernetes.io/metadata.name: ingress-nginx
  tenantNamespaceSelector:
    varroa.dev/tenant: "true"
  apiServerEgress:
    cidrs: ["10.0.0.2/32"]
    ports: [443, 6443]
  metricsIngress: []
```

Label every controller namespace selected by the policy:

```bash
kubectl label namespace jenkins-platform varroa.dev/tenant=true
helm upgrade varroa oci://ghcr.io/varroaci/charts/varroa \
  --version <version> -n varroa-system -f values.yaml
```

An empty `tenantNamespaceSelector` permits every namespace to reach the gateway. A non-empty selector is safer on shared clusters.

## Restrict External Egress

Git egress defaults to ports 443 and 22 for any destination. OCI and update-center rules default to HTTPS for any IPv4 address. Narrow them to known registry or proxy CIDRs:

```yaml
networkPolicy:
  ociRegistryEgress:
    enabled: true
    cidrs: ["10.20.0.0/16"]
    ports: [443]
  pullThroughEgress:
    enabled: false
  updateCenterRegistryEgress:
    enabled: true
    cidrs: ["10.20.0.0/16"]
    ports: [443]
```

Kubernetes NetworkPolicy matches IP addresses, not DNS names. Include every address returned by load-balanced endpoints. Update-center pull-through can contact both the configured update service and its checksum archive. Allow both or disable the archive fallback and pre-seed every required plugin.

## Verify Before Enforcement

Apply policies in a maintenance window and test actual flows:

```bash
kubectl get networkpolicy -n varroa-system
kubectl get pods -n varroa-system
kubectl get controller -A
```

Every running controller should return to `Connected`. Confirm dashboard login, bundle refresh, controller wake, metrics collection, and update-center readiness.

| Symptom | Likely cause |
|---|---|
| Controller remains `Running` | Tenant selector mismatch or TCP 9090 blocked. |
| Dashboard is unreachable | Ingress-controller selector mismatch. |
| Bundle fetch times out | Git or OCI egress blocked. |
| Login fails | Identity-provider egress blocked. |
| Update center is degraded | Registry or pull-through egress blocked. |

See [Air-gapped installation](air-gapped.md) for an offline allowlist.
