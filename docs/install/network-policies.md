# Configure Network Policies

The chart can apply default-deny ingress and egress policies to the Varroa release namespace. It is off by default and needs a CNI that enforces `NetworkPolicy`.

The chart does not isolate controller namespaces. Apply separate policies there for Jenkins and agent workloads.

## Inventory Required Traffic

| Source | Destination | Port | Purpose |
|---|---|---:|---|
| Controller namespaces | Gateway | 9090, 9092/TCP | mite and key checks |
| Controller pods, local issuer | BFF | 8080/TCP | Session keys |
| Control plane | NATS | 4222/TCP | Bus |
| Control plane | Kubernetes API | 443, 6443/TCP | Resources |
| Ingress controller | Dashboard, BFF | 8080/TCP | HTTP |
| Ingress controller | Operator | 8082/TCP | Wake |
| Operator, BFF | Jenkins | 8080/TCP | Operations |
| Recorder | NATS | 4222/TCP | Build event ingest |

External egress depends on enabled features:

| Source | Destination | Ports | Purpose |
|---|---|---:|---|
| Operator, BFF | Git | 443, 22/TCP | Bundles |
| Operator | OCI registry | 443/TCP | Artifacts |
| Dex or BFF | Identity provider | Provider ports | Login |
| Components | OTLP collector | 4317, 4318/TCP | Telemetry |
| Jenkins, control plane | Update center | 8080/TCP | Plugins |
| Recorder, BFF, gateway | Build record Postgres | 5432/TCP | Build records |
| Recorder, BFF, gateway | Build record blob storage | 443/TCP | Logs and artifacts |

The `networkPolicy.recordStoreBlobEgress` toggle governs the "Build record blob storage" row
above, for the recorder, the BFF, and the gateway, and for the operator in
[STS artifact mode](../operations/artifact-credentials.md#sts-mode). It renders nothing when
`recordStore.blobs.mode` is `postgres`, because that mode needs no external egress. See
[Build records](../operations/build-records.md#network-policy) for its defaults.

On a [member cluster](multi-cluster.md#record-store), the gateway policy carries the same blob rule.
Point it at the core object store address. In `postgres` blob mode the member gateway also needs
`networkPolicy.recordStorePostgresEgress.cidrs`, pinned to the core member endpoint. The core
database policy admits that traffic from `recordStore.postgres.memberEndpoint.allowedCIDRs`, a
required list when network policies are enabled. With `externalTrafficPolicy: Cluster`, the list must hold the core node and pod ranges. See the [multi-cluster page](multi-cluster.md#record-store).

An agent uploads build artifacts straight to the object store, not through the controller pod.
This chart does not isolate controller or agent namespaces. An agent namespace that egresses
through a restrictive policy of its own needs the same destination as the blob storage row
above, added to that namespace's own policy. See
[Build logs and artifacts](../operations/build-logs-and-artifacts.md#agent-namespace-egress).

Cluster DNS is also required. Add the build-time destinations of Jenkins and agents to tenant policies.

## Enable the Chart Policies

Set the selectors and API server addresses:

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

Label every controller namespace:

```bash
kubectl label namespace jenkins-platform varroa.dev/tenant=true
helm upgrade varroa oci://ghcr.io/varroaci/charts/varroa \
  --version <version> -n varroa-system -f values.yaml
```

An empty `tenantNamespaceSelector` permits every namespace to reach the gateway. A non-empty selector is safer on shared clusters.

## Restrict External Egress

Each egress group renders a rule only when its gate is on and its `cidrs` list is not empty. A gate that is on with an empty list fails the render. The failure message names the value to set.

Every group except `coreNatsEgress` also requires a non-empty `ports` list. An empty `ports` list fails the render, because Kubernetes reads it as every port.

| Group | Default | Renders when | Empty `cidrs` |
| --- | --- | --- | --- |
| `apiServerEgress` | `["0.0.0.0/0"]` on 443 and 6443 | Always | Fails the render. |
| `gitEgress` | `["0.0.0.0/0"]` on 443 and 22 | `enabled` is true | Fails the render while enabled. |
| `dexConnectorEgress` | `["0.0.0.0/0"]` on 443 | Dex is on and `enabled` is true | Fails the render while enabled. |
| `telemetryEgress` | `[]` on 4317 and 4318 | `telemetry.endpoint` is set | Fails the render while the endpoint is set. |
| `pullThroughEgress` | `["0.0.0.0/0"]` on 443 | `updateCenter.pullThrough.enabled` is true | Fails the render while pull-through is on. |
| `updateCenterRegistryEgress` | `[]` on 443 | `updateCenter.storage.type` is `oci` | Fails the render. |
| `ociRegistryEgress` | Off, `[]` on 443 | `enabled` is true | Fails the render. |
| `coreNatsEgress` | `[]` | Hive mode | Fails the render in hive mode. |
| `recordStoreBlobEgress` | On, `[]` on 443 | `recordStore.blobs.mode` is `s3` | Fails the render. |
| `recorderWebhookEgress` | Off, `[]` on 443 | `enabled` is true | Fails the render. |
| `assistantEgress` | `[]` on 443 | `assistant.enabled` is true | Fails the render when `cidrs` is empty. |
| `recordStorePostgresEgress` | On, `[]` on 5432 | `recordStore.enabled` is true | In `cnpg` mode, selects the CloudNativePG pods. In `external` mode, fails the render. |

The defaults of `apiServerEgress`, `gitEgress`, `dexConnectorEgress`, and `pullThroughEgress` allow every address on 443. Pin each one to the addresses that it reaches.

- `apiServerEgress`: pin each endpoint IP as a `/32` on port 6443. Get the IPs with `kubectl get endpointslice -n default -l kubernetes.io/service-name=kubernetes`. Calico matches the endpoint address, not the ClusterIP.
- `gitEgress`: pin the Git server and the non-Dex OIDC issuer.
- `dexConnectorEgress`: pin the identity provider addresses.
- `pullThroughEgress`: the upstream hosts are public CDN hosts. Pin the CIDRs of your mirror, or keep `["0.0.0.0/0"]`.

Egress rules add up. A connection is allowed when any rule allows it. The egress set closes only when you pin `apiServerEgress`, `gitEgress`, and `dexConnectorEgress`. While any of these keeps its default, the operator, BFF, and gateway reach any address on port 443.

Pin the groups that you control:

```yaml
networkPolicy:
  apiServerEgress:
    cidrs: ["10.0.0.2/32"]
    ports: [443, 6443]
  gitEgress:
    cidrs: ["10.0.1.0/24"]
    ports: [443, 22]
  dexConnectorEgress:
    cidrs: ["10.0.2.0/24"]
    ports: [443]
  ociRegistryEgress:
    enabled: true
    cidrs: ["10.20.0.0/16"]
    ports: [443]
  updateCenterRegistryEgress:
    cidrs: ["10.20.0.0/16"]
    ports: [443]
  recordStoreBlobEgress:
    cidrs: ["203.0.113.10/32"]   # s3 blob mode only
    ports: [443]
```

Kubernetes NetworkPolicy matches IP addresses, not DNS names. Include every address that a load-balanced endpoint returns. Update-center pull-through can contact the configured update service and its checksum archive at `repo.jenkins-ci.org`. The default pull-through rule allows both hosts.

### Empty Port Lists

Every group in the table except `coreNatsEgress` refuses `ports: []` while it is on. Set the TCP ports that the group needs.

### External NATS Clients

`nats.external.allowedCIDRs` is required when `networkPolicy.enabled` is true, full mode is active, and `nats.external.enabled` is true. An empty list fails the render. Set the member cluster source addresses. To admit every source, set `["0.0.0.0/0"]`.

Behind a NodePort or a LoadBalancer with `externalTrafficPolicy: Cluster`, kube-proxy in iptables or IPVS mode replaces the source address with a core node address. The allow-list must then include the core node range and pod range. Set `nats.external.externalTrafficPolicy: Local` to keep the member source address. Then list only the member egress addresses. The chart does not compute a default list, because it cannot know the member addresses.

### Upgrade Impact

With `networkPolicy.enabled: true`, an upgrade fails the render until you set the values below. No failure is silent.

1. `apiServerEgress`, `gitEgress`, or `dexConnectorEgress` with an empty `cidrs` list: set the addresses, or set `["0.0.0.0/0"]` to keep the wide default.
2. Any group in the table with an empty `ports` list: set the TCP ports.
3. `telemetry.endpoint` is set: set `networkPolicy.telemetryEgress.cidrs` to the collector address.
4. `nats.external.enabled` in full mode: set `nats.external.allowedCIDRs`.
5. `networkPolicy.dexConnectorEgress.extraCIDRs` is removed. Move its entries to `networkPolicy.dexConnectorEgress.cidrs`.
6. Update center with `updateCenter.storage.type: oci`: set `networkPolicy.updateCenterRegistryEgress.cidrs`.
7. Hive mode: set `networkPolicy.coreNatsEgress.cidrs`.
8. Record store with `recordStore.blobs.mode: s3`: set `networkPolicy.recordStoreBlobEgress.cidrs`.
9. `networkPolicy.recorderWebhookEgress.enabled: true`: set `cidrs`, or set `["0.0.0.0/0"]` to allow every address.
10. `pullThroughEgress`: no change.

## Isolate Cells

A [cell](../operations/cells.md) hosts many tenants on one controller, so it gets its own
`NetworkPolicy` instead of sharing the release-namespace policy above. The operator creates and
rebuilds this policy itself, one per cell, on every reconcile tick; the chart never renders it.

Enable it with:

```yaml
networkPolicy:
  enabled: true
  cellIsolation:
    enabled: true
    extraIngress: []
    extraEgress: []
```

`cellIsolation.enabled` only takes effect when `networkPolicy.enabled` is also `true`. Each
cell's policy admits the BFF and the operator on port 8080, and admits every currently placed
tenant's agent namespace on ports 8080 and 50000. It lets the cell reach cluster DNS, the
gateway's mite gRPC and API-key-verify ports, the in-cluster update center on port 8080, the
configured Kubernetes API server addresses, and every placed tenant's agent namespace.

The fixed rules admit no blob store. An S3 endpoint on port 443 is reachable through the default
`apiServerEgress` rule. An endpoint on another port, such as an in-cluster MinIO on 9000, needs
one `cellIsolation.extraEgress` entry, or the cell's artifact uploads fail.

`cellIsolation.extraIngress` and `cellIsolation.extraEgress` take raw `NetworkPolicy` rule
entries, appended after the fixed rules above. Use them for a plugin that needs its own outbound
call, such as a notification integration.

A cell's policy also admits one egress rule per [`ssh` external runner](../operations/runners.md#cell-egress-for-an-ssh-host)
of a tenant currently placed on it, to that runner's `spec.host` on `spec.port`. The operator
computes these rules itself on every reconcile; `cellIsolation.extraEgress` is a separate,
hand-maintained list and plays no part in them.

A cell's policy also admits one egress rule per resolved address of each pool library remote.
The operator resolves the remote host on every reconcile. See
[pool library egress](../operations/cell-libraries.md#egress).

## The public host and WebSocket upgrades

The dashboard host serves both the tenant webhook door and the external-runner agent door. Any
reverse proxy placed in front of the ingress, in addition to the ingress itself, must forward
the `Upgrade` and `Connection` headers and allow a long-lived connection. Otherwise an
`inbound` external runner's WebSocket connection to the [agent door](../operations/runners.md#register-an-inbound-runner)
cannot open.

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
