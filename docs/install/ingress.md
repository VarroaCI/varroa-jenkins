# Ingress

<!-- sources: api/v1alpha1/types.go (IngressSpec), internal/controller/controller_controller.go (reconcileIngress), charts/varroa/templates/ingress, charts/varroa/values.yaml -->

Varroa needs ingress in two places: one host for the dashboard, and exposure for each Jenkins controller. This page covers both, plus TLS and per-controller customization.

## Concepts

- **Dashboard ingress** — created by the Helm chart. Browsers only ever need this one hostname: the frontend's nginx proxies API calls (`/api/`, `/.well-known/`) to the BFF internally.
- **Controller ingress** — created by the operator, one per controller, in the controller's namespace. Two modes:
  - `subdomain` (default): each controller gets its own host, `<controller>.<rootDomain>`.
  - `path`: the controller is served at `/jenkins/<namespace>/<name>` on a shared host — one wildcard-free certificate, no per-controller DNS. The mode is **immutable after create**.
- **The gateway needs no ingress.** Mites are sidecars inside the cluster and reach the gateway through its ClusterIP Service on `:9090`.
- Annotations and ingress class resolve in layers: cluster-wide defaults from `ProvisioningDefaults` (`ingressAnnotations`, `ingressClassName`), overridden per controller by `spec.ingressSpec` — on key conflict the controller wins.

> [!IMPORTANT]
> The dashboard host must serve valid TLS — login cookies are `Secure` and browsers drop them over plain HTTP. Controller hosts should too; SSO into Jenkins uses the same cookie domain.

## How to set brood-wide ingress defaults

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ProvisioningDefaults
metadata:
  name: varroa-defaults
spec:
  rootDomain: example.com               # controllers become <name>.example.com
  ingressClassName: nginx
  ingressAnnotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
```

```bash
kubectl apply -f defaults.yaml
```

**Verify:** newly provisioned controllers get an Ingress with your class and annotations: `kubectl get ingress -n <controller-ns> <controller> -o jsonpath='{.metadata.annotations}'`.

## How to customize one controller's ingress

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: demo
  namespace: teams-platform
spec:
  namespace: teams-platform
  version: "2.516.1"
  ingressSpec:
    host: ci-demo.example.com           # explicit host instead of <name>.<rootDomain>
    tlsSecretName: ci-demo-tls          # existing TLS secret, or cert-manager-created
    ingressClassName: nginx-internal    # override the cluster default
    annotations:
      nginx.ingress.kubernetes.io/proxy-body-size: 512m   # merged over defaults; this value wins
```

```bash
kubectl apply -f controller.yaml
```

**Verify:** `kubectl get ingress -n teams-platform demo -o jsonpath='{.spec.rules[0].host}'` prints `ci-demo.example.com` and the TLS section references `ci-demo-tls`.

## How to use path mode (shared host)

Choose path mode **at creation time** (it's immutable afterwards):

```yaml
spec:
  ingressSpec:
    mode: path
```

The controller is then served at `https://<shared-host>/jenkins/<namespace>/<name>/`. This avoids per-controller DNS records and wildcard certificates — useful in restrictive DNS environments — at the cost of path-prefixed URLs.

**Verify:** the controller's Ingress rule uses the shared host with path `/jenkins/<namespace>/<name>`, and the Jenkins root URL reflects the prefix.

## Concepts: live convergence

Ingress is reconciled on **every** tick for controllers in `Running`/`Connected` — not just during provisioning. Changing `ingressSpec` (a new TLS secret, an annotation) converges in place within seconds, with no pod restart and no re-provision.

**Verify:** edit `ingressSpec.tlsSecretName` on a `Connected` controller and watch `kubectl get ingress -n <ns> <name> -o yaml` update within one reconcile interval (default 30s).

## Troubleshooting

- Ingress created but 404s → wrong `ingressClassName` for your ingress controller; check `kubectl get ingressclass`.
- Login works on the dashboard but Jenkins SSO fails → `auth.cookieDomain` doesn't cover the controller host; see [Authentication](../security/authentication.md).
- TLS change not converging → controller must be in `Running`/`Connected`; check `status.phase` and [Troubleshooting](../operations/troubleshooting.md).

## Related pages

- [Prerequisites](prerequisites.md) — DNS and cert-manager groundwork
- [Network policies](network-policies.md) — allowing ingress-controller traffic
- [Pod customization](../config/pod-customization.md) — raw `resourceOverlay.ingress` for cases `ingressSpec` doesn't cover
