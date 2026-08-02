# <img src="assets/varry-mascot.svg" width="48" height="48" alt="Varry" align="top"/> Varroa

The open-source alternative to CloudBees CI — a Kubernetes-native operator for managing
Jenkins controllers at scale, with built-in SSO, Git-backed configuration, and brood observability.

Meet **Varry**, the Varroa mite who keeps your Jenkins brood buzzing.

[![PR](https://github.com/varroaci/varroa-jenkins/actions/workflows/pr.yaml/badge.svg)](https://github.com/varroaci/varroa-jenkins/actions/workflows/pr.yaml)

- **Multi-cluster from one control plane** — controllers on remote clusters, one dashboard, one RBAC model.
- **Air-gapped by design** — an in-cluster Jenkins update center serving digest-verified plugins from OCI storage, with no egress.
- **Git and OCI as configuration sources** — JCasC composed from ordered inputs, not copy-pasted per controller.
- **Fleet operations** — restart, upgrade, or run a script across a brood in staged waves with a canary.

### You don't outgrow it

The same `Controller` resource describes one Jenkins and five hundred. Nothing is
rewritten on the way up; things are added as you need them:

| At this point | You reach for |
|---|---|
| First controller | Nothing. `kubectl apply` a `Controller` — it uses a built-in starter bundle |
| A team wants their own config | A [`ComposedBundle`](docs/config/composed-bundles.md) pointed at their git repo |
| Several teams, one baseline | [Catalog items](docs/config/casc-catalog.md) — shared fragments, ordered composition |
| Many controllers, same shape | A [`ControllerClass`](docs/config/controller-classes.md) |
| Upgrades need to be deliberate | [`JenkinsVersionProfile`](docs/config/jenkins-versions.md) — pinned core + plugin lock |
| Changes need to land safely | [Rollout waves](docs/operations/rollout-waves.md) |
| No egress allowed | The [update center](docs/install/air-gapped.md) |
| More than one cluster | [Multi-cluster](docs/install/multi-cluster.md) |

Each row is optional and independent. Adopting one does not commit you to the next.

## What is Varroa?

Varroa is a Kubernetes operator that manages the full lifecycle of Jenkins controllers.
You define a `Controller` custom resource, and Varroa provisions a StatefulSet running
Jenkins with a **mite** sidecar agent. The mite connects back to the gateway over gRPC
(mTLS). The operator publishes desired-state commands through NATS, and the gateway
delivers them to the mite: JCasC configuration, plugin manifests, and RBAC policies.
The operator continuously reconciles — when the desired state diverges from the mite's
reported snapshot, it publishes an update.

Authentication uses any RFC-compliant **OIDC provider** (Auth0, Okta, Azure AD,
Keycloak, Google, etc.). Varroa issues a `varroa_token` cookie on a shared parent
domain so every Jenkins controller sees it. For upstream identity providers that
don't speak OIDC natively (GitHub OAuth, LDAP, SAML), Varroa can deploy **Dex** as
a federation broker to translate between protocols. The **VarroaSecurityRealm**
plugin (a Jenkins HPI in plugin/) validates the JWT and maps OIDC group claims to
Jenkins roles. Unauthenticated users are redirected to Varroa's login page — no
per-controller redirect URIs needed.

The mite sidecar holds no Jenkins credentials at rest: the operator signs an RS256
JWT and pushes it to the mite over the command stream, and the in-repo
**VarroaSecurityRealm** plugin verifies it offline against the operator's public key.
No API tokens, no init scripts — details in the
[mite documentation](docs/architecture/mite.md).

## Architecture

```
Browser → Ingress → Frontend → BFF HTTP API/SSE → Kubernetes API + NATS read models
                                  │
                                  └── OIDC provider login (Dex optional) and varroa_token cookie

Controller CRDs → Operator reconciler → NATS JetStream/KV → Gateway → gRPC mTLS → mite
                      │                                      │                  │
                      └── Kubernetes provisioning             └── live streams   └── Jenkins API

Observability: Prometheus → Grafana dashboards (optional, bundled)
```

The **mite** sidecar runs in every Jenkins pod. It registers with the gateway via a
bootstrap HMAC token, receives an mTLS client certificate from the internal CA, and
opens a long-lived bidirectional `CommandStream`. The gateway bridges stream traffic to
NATS; the operator owns reconciliation and desired-state publication, while the BFF owns
the frontend-facing HTTP API and read models.

## Installation

> **New to Varroa?** Start with the [Operator Handbook](docs/README.md) — the
> [first-controller tutorial](docs/tutorials/first-controller.md) walks from zero to a
> running, managed Jenkins.

Varroa is deployed via Helm. To evaluate it you need a Kubernetes cluster (v1.30+)
with a default StorageClass. To run it for other people you also need `cert-manager`
for TLS, a domain name, and an identity provider — see
[Prerequisites](docs/install/prerequisites.md), which splits the two.

```bash
helm install varroa oci://ghcr.io/varroaci/charts/varroa --version 0.1.0 \
  --namespace varroa-system \
  --create-namespace \
  --values values.yaml
```

Minimal `values.yaml` — choose the variant that fits your identity provider:

<details>
<summary><b>Option A — Direct OIDC</b> (Auth0, Okta, Azure AD, Keycloak, Google, etc.)</summary>

```yaml
global:
  domain: your-domain.com

dex:
  enabled: false

auth:
  mode: oidc
  cookieDomain: .your-domain.com
  oidc:
    issuer: https://dev-xxx.us.auth0.com/
    clientId: <oidc-client-id>
    clientSecret: <oidc-client-secret>
    redirectUrl: https://varroa.your-domain.com/callback

ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-route53
  tls:
    - hosts: [varroa.your-domain.com]
      secretName: varroa-tls
```
</details>

<details>
<summary><b>Option B — Dex as federation broker</b> (GitHub OAuth, LDAP, SAML)</summary>

```yaml
global:
  domain: your-domain.com

dex:
  enabled: true
  config:
    issuer: https://dex.your-domain.com
    connectors:
      - type: github
        id: github
        name: GitHub
        config:
          clientID: $GITHUB_CLIENT_ID
          clientSecret: $GITHUB_CLIENT_SECRET
          orgs:
            - name: your-github-org
          loadAllGroups: true
          claims:
            groups: ["groups"]

ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-route53
  tls:
    - hosts: [varroa.your-domain.com, dex.your-domain.com]
      secretName: varroa-tls
```
</details>

See [Installing with Helm](docs/install/helm-install.md) for full configuration details, and
[Authentication](docs/security/authentication.md) for all four auth modes (direct OIDC,
Dex-brokered, native LDAP, local).

### ProvisioningDefaults

Set cluster-wide defaults before creating controllers:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ProvisioningDefaults
metadata:
  name: varroa-defaults
spec:
  storageClass: openebs-hostpath
  ingressClassName: nginx
  imagePullSecrets:
    - ghcr-credentials
  ingressAnnotations:
    cert-manager.io/cluster-issuer: letsencrypt-route53
```

## Quickstart

The smallest controller Varroa accepts:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: demo
  namespace: varroa
spec:
  namespace: varroa
```

No bundle and no version. It uses the starter bundle the operator ships — a system
message, a Kubernetes cloud so builds get agents, and one sample pipeline — running
the Jenkins core that matches Varroa's embedded plugin lock. With no ingress host
configured it is reachable with `kubectl port-forward` and says so in its status.
See the [first-controller tutorial](docs/tutorials/first-controller.md).

Once you want your own configuration, name a bundle and pin a version:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ComposedBundle
metadata:
  name: standard
  namespace: varroa-tenants
spec:
  inputs:
    - gitSource:
        repoURL: https://github.com/your-org/jcasc-bundles.git
        path: bundles/standard
        revision: main
---
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: team-web-builds
  namespace: varroa-tenants
spec:
  namespace: varroa-tenants
  version: "2.555"                 # governed by JenkinsVersionProfile objects
  composedBundleRef:
    name: standard
  ingressSpec:
    host: builds.team-web.your-domain.com
    tlsSecretName: team-web-builds-tls
  resources:
    storage: 20Gi
```

Apply it:

```bash
kubectl apply -f controller.yaml
kubectl get controllers -n varroa-tenants
```

The controller moves through phases: `Pending → Provisioning → Running → Connected`.
You can also use the REST API:

```bash
curl -X POST https://varroa.your-domain.com/api/v1/controllers \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $VARROA_TOKEN" \
  -d @controller.json
```

## CRDs

| Resource | Scope | Purpose |
|----------|-------|---------|
| [`Controller`](docs/operations/lifecycle.md) | Namespaced | A managed Jenkins instance |
| [`ComposedBundle`](docs/config/composed-bundles.md) | Namespaced | Ordered composition of JCasC inputs into one bundle |
| [`CatalogSource`](docs/config/casc-catalog.md) | Namespaced | Git-synced source of reusable catalog items |
| [`CatalogItem`](docs/config/casc-catalog.md) | Namespaced | One reusable, parameterized config snippet (operator-owned) |
| [`JenkinsVersionProfile`](docs/config/jenkins-versions.md) | Cluster | Pinned plugin set + JCasC overlay per Jenkins version/LTS line |
| [`ProvisioningDefaults`](docs/install/helm-install.md) | Cluster | Brood defaults (storage, ingress, plugins, namespaces, presets) |
| [`VarroaRole`](docs/security/varroa-rbac.md) / [`VarroaRoleBinding`](docs/security/varroa-rbac.md) | Cluster | Control-plane (API/dashboard) authorization |
| [`JenkinsRole`](docs/security/jenkins-rbac.md) / [`JenkinsRoleBinding`](docs/security/jenkins-rbac.md) | Cluster | In-Jenkins permissions, generated into role strategy |
| [`Team`](docs/operations/multi-tenancy.md) | Cluster | Tenant team: namespaces + scoped RBAC |
| [`Group`](docs/security/varroa-rbac.md) | Cluster | Identity group used by RBAC subjects |
| [`User`](docs/security/authentication.md) | Namespaced | Local-auth user record |
| [`PodTemplate`](docs/config/pod-customization.md) | Namespaced | Kubernetes pod template for Jenkins agents |
| [`BuildMetric`](docs/operations/observability.md) | Namespaced | Per-build metrics collected by the mite |

## Auth Model

Varroa replaces Jenkins' native authentication entirely:

1. **User login** — Browser redirects to `https://varroa.your-domain.com/login`. Varroa's
   OIDC handler redirects to the OIDC provider (Auth0, Okta, Azure AD, etc.) — or to Dex
   first when using it as a federation broker for GitHub OAuth, LDAP, or SAML.
2. **JWT issuance** — The OIDC provider issues an ID token with email, name, and group claims.
3. **Cookie** — Varroa sets `varroa_token` as a domain cookie (`.your-domain.com`), so
   every Jenkins controller at `*.your-domain.com` receives it.
4. **VarroaSecurityRealm** — A Jenkins plugin (HPI) validates the JWT against the OIDC
   provider's JWKS endpoint and creates a Jenkins authentication with group authorities.
5. **Mite auth** — The mite presents an operator-signed RS256 JWT as a Bearer token;
   the VarroaSecurityRealm verifies it offline against the operator's public key
   (no Dex or network dependency) and grants the mite's minimal role.

No shared secrets, no CSRF hacks. The auth plugin lives in this repo under
[`plugin/`](plugin/) and is delivered into each Jenkins by an init container.
Full detail: [Authentication](docs/security/authentication.md).

## RBAC Model

Authorization is split into two layers, both declared as cluster-scoped CRDs:

- **[Varroa RBAC](docs/security/varroa-rbac.md)** — `VarroaRole`/`VarroaRoleBinding` govern
  the dashboard and REST API. Built-in roles: `admin`, `operator`, `developer`, `viewer`.
- **[Jenkins RBAC](docs/security/jenkins-rbac.md)** — `JenkinsRole`/`JenkinsRoleBinding`
  govern permissions *inside* each Jenkins, generated into the role-strategy config.
  Varroa owns the authorization strategy; bundles can't override it.

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata:
  name: platform-operators
spec:
  roleRef: operator
  subjects:
    - kind: Group
      name: "acme:platform-team"
```

Bindings scope to namespaces and controller label selectors; Jenkins-side bindings
additionally scope to folders or item-name patterns.

See [Plugin pinning](docs/config/plugin-pinning.md) for plugin governance and
[Multi-tenancy](docs/operations/multi-tenancy.md) for brood/team management.

## Observability

Varroa exports Prometheus metrics from all services. Key metrics:

| Metric | Description |
|--------|-------------|
| `varroa_controller_status` | Provisioning status per controller |
| `varroa_controller_provision_seconds` | Time to provision a controller |
| `varroa_reconciliation_total` | Reconciliation loop executions |
| `varroa_jenkins_health` | Jenkins instance health |

The Helm chart includes Prometheus and Grafana (optional). Grafana dashboards show
controller inventory, provisioning timelines, plugin version matrices, and resource
utilization across the brood. A JetStream-backed activity feed narrates every brood
event — see [Observability](docs/operations/observability.md).

## Development

### Prerequisites

| Tool    | Version | Check |
|---------|---------|-------|
| Go      | 1.25+   | `go version` |
| Node.js | 20+     | `node --version` |
| kubectl | 1.30+   | `kubectl version --client` |
| Helm    | 3.17+   | `helm version` |
| Kind    | 0.27+   | `kind version` |
| Docker  | 26.0+   | `docker version` |

### Local development cluster

```bash
make localdev        # full stack on a disposable kind cluster, browser-ready
make localdev-images # rebuild + roll after code changes
make localdev-down   # tear down
```

One command stands up operator, gateway, BFF, frontend, NATS, HTTPS ingress, and a
sample Jenkins controller at `https://app.varroa.localtest.me` (login
`admin` / `password` — built-in local auth). Details, WSL2 notes, and troubleshooting:
[Local development](docs/install/local-development.md).

### Backend

```bash
make build          # → bin/varroa-operator, bin/varroa-gateway, bin/varroa-bff, bin/varroa-mite
make test           # all Go tests with race detector
make lint           # golangci-lint
make generate-crds  # regenerate CRD YAML from Go type markers
```

The split backend components are `varroa-operator` (reconciliation), `varroa-gateway`
(mite gRPC on `:9090`), and `varroa-bff` (HTTP API on `:8080`).

### Frontend

```bash
cd frontend && npm ci && npm run dev   # Vite at :3000
```

Set `VITE_VARROA_BFF_URL=http://localhost:8080/api/v1` in `frontend/.env`.

### Rebuild and deploy

```bash
CONTROLLER_NAME=v20 bash scripts/rebuild-and-deploy.sh
```

This builds the operator image, pushes to `ghcr.io`, updates the gitops repo, and
creates a test controller. Requires `GITOPS_REPO` to be set.

### Project structure

See [CLAUDE.md](CLAUDE.md) for detailed architecture documentation, the controller
lifecycle, mite protocol, and code layout.

## Contributing

Issues and PRs welcome. See the open issues (and the [roadmap](docs/roadmap.md)) for
areas to contribute. The operator and mite are written in Go; the auth plugin is a
Jenkins HPI under [`plugin/`](plugin/).

All contributions must be signed off under the
[Developer Certificate of Origin](https://developercertificate.org/)
(`git commit -s`).

## License

Varroa is licensed under the [GNU Affero General Public License v3.0](LICENSE)
(AGPL-3.0). You are free to use, modify, and self-host Varroa. If you offer a
modified version of Varroa to others over a network, the AGPL requires you to
make your modified source available to those users.

