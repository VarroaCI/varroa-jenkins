# Install with Helm

The OCI chart installs the operator, gateway, BFF, dashboard, NATS JetStream, and CRDs. The operator seeds a set of default Jenkins version profiles itself on start. See [Jenkins Versions](../config/jenkins-versions.md#default-profiles). Dex is enabled by default. Network policies and the in-cluster update center are opt-in.

Complete [Prerequisites](prerequisites.md) first.

## Prepare Values

This example uses a direct OIDC provider and nginx ingress:

```yaml
global:
  domain: example.com

managedNamespaces:
  - jenkins-platform

auth:
  mode: oidc
  dashboardUrl: https://app.example.com
  cookieDomain: .example.com
  oidc:
    issuer: https://login.example.com/
    clientId: varroa
    clientSecret: "<client-secret>"
    redirectUrl: https://app.example.com/api/v1/callback

dex:
  enabled: false

ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  tls:
    - hosts: [app.example.com]
      secretName: varroa-dashboard-tls
```

Create every namespace in `managedNamespaces` before creating controllers. An empty list allows the operator and BFF to manage workloads in any namespace. Register the exact redirect URL with the identity provider and protect the values file because it contains credentials.

See [Ingress](ingress.md) and [Authentication](../security/authentication.md) for other routing and identity modes.

## Install

```bash
kubectl create namespace jenkins-platform
helm install varroa oci://ghcr.io/varroaci/charts/varroa \
  --version <version> \
  -n varroa-system --create-namespace \
  -f values.yaml
```

Pin a released chart version.

## Verify

```bash
helm status varroa -n varroa-system
kubectl get pods,pvc -n varroa-system
kubectl get crd -o name | grep 'varroa.dev'
kubectl get jenkinsversionprofile
```

Wait for all control-plane pods to become `Ready`. NATS cannot start until its volumes bind. The dashboard denies actions until the signed-in identity has a [Varroa role binding](../security/varroa-rbac.md).

## Important Values

| Area | Values |
|---|---|
| Namespace scope | `managedNamespaces` |
| Authentication | `auth.*`, `dex.*` |
| Dashboard routing | `global.domain`, `frontend.host`, `ingress.*` |
| Control-plane capacity | `operator.*`, `gateway.hpa.*`, `bff.hpa.*` |
| NATS durability | `nats.config.cluster.*`, `jetStreamReplicas` |
| Activity retention | `recordStore.retention.activity` |
| Network isolation | `networkPolicy.*` |
| Plugin service | `updateCenter.*` |
| Build record store | `recordStore.*` |
| Build recorder component | `recorder.*` |
| Build record store network isolation | `networkPolicy.recordStore*` |
| Cell pools | `cells.*` |
| Telemetry | `telemetry.*` |

Gateway and BFF HPAs require the Kubernetes metrics API. Telemetry export remains inactive until `telemetry.endpoint` is set.

Each `cells.*` value seeds a `CellPool` the operator creates. None of them configures a live pool,
and an upgrade never rewrites the spec of a pool that already exists. Scale a live pool by editing
the `CellPool` object. See [Cells and pipelines tenants](../operations/cells.md).

### BFF ConfigMap Access

The BFF is the internet-facing component. Its ClusterRole grants no `configmaps` rule in any mode,
so the BFF cannot read every ConfigMap in the cluster. It reaches a ConfigMap through two channels
only.

The first channel is a Role in the release namespace. It covers the plugin sets that version
profiles materialize.

The second channel is a per-bundle grant. The operator writes a `Role` and a `RoleBinding` named
`<bundle>-content-reader` beside each composed bundle's content ConfigMap, in that bundle's own
namespace. The Role grants `get` on that one ConfigMap by name. It grants no `list` and no `watch`.
Both objects carry the bundle's owner reference, so Kubernetes removes them with the bundle.

The operator learns the subject from the `VARROA_BFF_SERVICE_ACCOUNT` variable, which the chart
sets on the operator deployment. A hive install deploys no BFF. The chart leaves the variable
unset there, and the operator writes no grant.

The operator needs `configmaps` `get` and `roles` write in each namespace where it reconciles a
bundle. Kubernetes escalation prevention permits a Role write only for permissions the writer
already holds. Scoped mode keeps both grants in the release namespace and in every listed
namespace, so the grant still works.

The build record store defaults to off (`recordStore.enabled: false`). When enabled,
`recordStore.postgres.mode` defaults to `cnpg` and needs the CloudNativePG operator installed
first; `recordStore.blobs.mode` defaults to `postgres`; and `recordStore.retention` defaults to
`runs: 365d`, `logs: 90d`, `artifacts: 30d`, `activity: 30d`. Each retention value takes the form
`<n>d`, a whole number of days from `1` to `3650`. `recorder.replicas` defaults to `1`.
`networkPolicy.recordStorePostgresEgress` and `networkPolicy.recordStoreBlobEgress` both default
to `enabled: true`, on ports `5432` and `443`. See [Build records](../operations/build-records.md)
for the CloudNativePG prerequisite and the full value tree.

Set `recordStore.blobs.mode: s3` to store console logs and archived artifacts in an
S3-compatible bucket instead of Postgres. Artifact storage needs this mode. Postgres mode keeps
logs but leaves artifacts on the controller's local disk.

The value `recordStore.blobs.s3.artifactSecret` names a base Secret the operator reads once per
controller (`<artifactSecret>-<controller>`) and projects into that controller's namespace, so
each cell writes with its own scoped credential. See
[Build logs and artifacts](../operations/build-logs-and-artifacts.md) for the per-cell prefix,
the required bucket policy, and the Secret shape.

## Use an Image Mirror

Mirror the component images and configure pull secrets as described in
[Configure Internal Images](air-gapped.md#configure-internal-images). Also mirror
enabled dependencies, Jenkins images, agent images, and plugin artifacts.

## Upgrade

Helm does not upgrade CRDs from a chart's `crds/` directory. Pull the new chart, apply its CRDs, then upgrade the release:

```bash
CHART_DIR=$(mktemp -d)
helm pull oci://ghcr.io/varroaci/charts/varroa \
  --version <version> --untar --untardir "$CHART_DIR"
kubectl apply --server-side -f "$CHART_DIR/varroa/crds/"
helm upgrade varroa oci://ghcr.io/varroaci/charts/varroa \
  --version <version> -n varroa-system -f values.yaml
```

Verify the Helm revision, control-plane pods, and existing controllers. Upgrading the control plane does not change `Controller.spec.version`.

### Bus credentials and GitOps renders

The chart mints the NATS passwords and TLS material once. On `helm upgrade` it
re-reads them from the existing `<release>-nats-creds` Secret. Renderers that
cannot read the cluster mint fresh values on every render. This applies to
`helm template`, ArgoCD, and Flux.

Fresh values are safe. The control-plane components read their password from the
mounted Secret on every reconnect. After a rotation they reconnect with no
restart, once kubelet has synced the new Secret onto the pod. That sync takes
about a minute.

During that window the operator reports NotReady, because `/readyz` fails while
the bus is down, and the `varroa.bus.connected` gauge drops to 0. Both recover on
their own.

Restarting a pod during that window is also safe. A component that starts before
its Secret has synced waits for the credential rather than exiting, so it sits
NotReady until the bus accepts it.

## Uninstall

Delete controllers before removing the operator so finalizers can clean up managed resources:

```bash
kubectl delete controller --all -A
helm uninstall varroa -n varroa-system
```

Inspect retained PVCs before deleting them. Deleting a CRD also deletes all custom resources of that kind.

Continue with [Your first controller](../tutorials/first-controller.md).
