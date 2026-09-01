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
| Activity retention | `activity.*` |
| Network isolation | `networkPolicy.*` |
| Plugin service | `updateCenter.*` |
| Telemetry | `telemetry.*` |

Gateway and BFF HPAs require the Kubernetes metrics API. Telemetry export remains inactive until `telemetry.endpoint` is set.

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

## Uninstall

Delete controllers before removing the operator so finalizers can clean up managed resources:

```bash
kubectl delete controller --all -A
helm uninstall varroa -n varroa-system
```

Inspect retained PVCs before deleting them. Deleting a CRD also deletes all custom resources of that kind.

Continue with [Your first controller](../tutorials/first-controller.md).
