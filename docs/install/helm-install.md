# Install with Helm

The OCI chart installs the operator, gateway, BFF, dashboard, NATS JetStream, and CRDs. The operator seeds default Jenkins version profiles on start. See [Jenkins Versions](../config/jenkins-versions.md#default-profiles). Dex is enabled by default. Network policies and the in-cluster update center are opt-in.

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
| NATS server and storage | `nats.container.image`, `nats.config.cluster.*`, `nats.config.jetstream.fileStore.pvc.*`, `jetStreamReplicas` |
| Activity retention | `recordStore.retention.activity` |
| Restart copy retention | `recordStore.retention.restartSource` |
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
the `CellPool` object. See [Cells and pipelines tenants](../operations/cells.md). Helm refuses a
`cells.maxTenantsPerCell` below 1 and a `cells.warmPool.perProfile` below 1 at render time. Each
of the two must be a whole number that fits a 32-bit integer.

`cells.cellTemplate` seeds the resources and node spread of the cells in a new pool. See
[Default template](../operations/cell-pools.md#default-template).

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
`runs: 365d`, `logs: 90d`, `artifacts: 30d`, `activity: 30d`, `restartSource: 7d`. Each retention
value takes the form `<n>d`, a whole number of days from `1` to `3650`. `restartSource` must not
be longer than `runs`. `recorder.replicas` defaults to `1`.
`networkPolicy.recordStorePostgresEgress` and `networkPolicy.recordStoreBlobEgress` both default
to `enabled: true`, on ports `5432` and `443`. `recordStoreBlobEgress.cidrs` has no default. See [Build records](../operations/build-records.md)
for the CloudNativePG prerequisite and the full value tree.
`recordStore.postgres.memberEndpoint` exposes the CloudNativePG primary to member clusters. It
defaults to off. A member cluster sets `recordStore.enabled: true` with
`recordStore.postgres.mode: external` and renders no database. See
[Record store](multi-cluster.md#record-store) for the member values.

Set `recordStore.blobs.mode: s3` to store console logs and archived artifacts in an
S3-compatible bucket instead of Postgres. Artifact storage needs this mode. Postgres mode keeps
logs but leaves artifacts on the controller's local disk.

Set `recordStore.blobs.s3.artifactSecret` for static artifact credentials, or
`recordStore.blobs.s3.artifactSTS` for credentials the operator issues per cell. See
[Artifact credentials](../operations/artifact-credentials.md).

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

To keep earlier values, use `--reset-then-reuse-values`. The chart refuses `--reuse-values`, which drops the encryption defaults.

### Bus credentials and GitOps renders

The chart mints the NATS passwords, the TLS material, and the JetStream
encryption key once and re-reads them on `helm upgrade`. Renderers that cannot
read the cluster (`helm template`, ArgoCD, Flux) mint fresh values on each render.
Fresh passwords are safe: the components recover in about a minute. A fresh key
is not: NATS encrypts JetStream with the key in `varroa-nats-js-key`, and a
changed or lost key loses all JetStream state. Back up the key after the first
install:

```bash
kubectl get secret varroa-nats-js-key -n varroa-system -o jsonpath='{.data.key}' | base64 -d
```

For a GitOps render, set `nats.encryption.createSecret: false` and create the
Secret yourself (a letter first, then letters and digits only):

```bash
kubectl create secret generic varroa-nats-js-key -n varroa-system --from-literal=key=<key>
```

Uninstall keeps the Secret. Without it `helm upgrade` fails; restore it from the backup.

## Uninstall

Delete controllers before removing the operator so finalizers can clean up managed resources:

```bash
kubectl delete controller --all -A
helm uninstall varroa -n varroa-system
```

Inspect retained PVCs before deleting them. Deleting a CRD also deletes all custom resources of that kind.

Continue with [Your first controller](../tutorials/first-controller.md).
