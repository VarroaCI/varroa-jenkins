# Configure Ingress

Helm creates the dashboard Ingress. The operator creates one Ingress for each controller that resolves an external host. The mite gateway remains cluster-internal.

## Choose a Controller Route

| Mode | Route | Use when |
|---|---|---|
| `subdomain` | `https://<controller>.<root-domain>/` | Wildcard DNS and certificates are available. |
| `path` | `https://<dashboard-host>/jenkins/<namespace>/<controller>/` | One hostname and shared ingress are preferred. |

`subdomain` is the default. `spec.ingressSpec.mode` is immutable after controller creation.

Path mode uses the dashboard host and its TLS configuration. Do not set `tlsSecretName` on a path-mode controller. Varroa configures the Jenkins URL prefix automatically.

## Expose the Dashboard

```yaml
global:
  domain: example.com
frontend:
  host: app.example.com
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  tls:
    - hosts: [app.example.com]
      secretName: varroa-dashboard-tls
auth:
  dashboardUrl: https://app.example.com
  cookieDomain: .example.com
```

Use HTTPS. Set `auth.cookieDomain` to the shared parent domain when dashboard authentication must work on controller subdomains.

## Set Controller Defaults

The cluster-scoped object must be named `varroa-defaults`:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ProvisioningDefaults
metadata:
  name: varroa-defaults
spec:
  rootDomain: example.com
  ingressClassName: nginx
  ingressAnnotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
```

```bash
kubectl apply -f defaults.yaml
```

A subdomain controller without an explicit host then uses `<name>.example.com`.

## Override One Controller

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: build
  namespace: jenkins-platform
spec:
  ingressSpec:
    host: build.example.com
    ingressClassName: nginx
    tlsSecretName: build-tls
    annotations:
      nginx.ingress.kubernetes.io/proxy-body-size: 512m
```

Controller annotations merge over `ProvisioningDefaults.spec.ingressAnnotations`. Controller values win on conflicts.

For path routing:

```yaml
spec:
  ingressSpec:
    mode: path
    host: app.example.com
```

If no host can be resolved, Varroa creates no controller Ingress and sets the informational `NoExternalURL` condition. The in-cluster Service and port forwarding remain available.

## Verify

```bash
kubectl get ingress -A
kubectl get controller build -n jenkins-platform \
  -o jsonpath='{.status.endpoint}{"\n"}'
```

Ingress host, class, annotations, and TLS changes converge without restarting Jenkins.

| Symptom | Check |
|---|---|
| 404 response | Host rule, ingress class, and ingress-controller events. |
| Login loop | HTTPS, OIDC callback URL, and cookie domain. |
| No controller Ingress | `rootDomain`, controller host, and `NoExternalURL`. |
| Path route redirects incorrectly | Dashboard host and immutable path mode. |

See [Network policies](network-policies.md) and [Authentication](../security/authentication.md).
