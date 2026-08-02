# Prerequisites

<!-- sources: charts/varroa/Chart.yaml, charts/varroa/values.yaml, charts/varroa/templates/networkpolicy.yaml -->

What you need before installing Varroa. Missing something here is the most common cause of getting stuck later.

Two tiers, because they are genuinely different lists. **Evaluate** is what [your first controller](../tutorials/first-controller.md) needs and nothing more. **Production** adds what a shared installation needs — most of which exists so that people other than you can reach it safely.

## Cluster

| | Evaluate | Production |
|---|---|---|
| **Kubernetes ≥ 1.30** (enforced by the chart's `kubeVersion`) | required | required |
| **A default StorageClass** — every Jenkins controller gets a PersistentVolumeClaim for `$JENKINS_HOME`; NATS JetStream also uses file storage | required | required |
| **An ingress controller** — e.g. [ingress-nginx](https://kubernetes.github.io/ingress-nginx/) | not needed | required |
| **TLS certificates** — [cert-manager](https://cert-manager.io/) is the usual choice | not needed | required |
| **DNS you control** | not needed | required |

Without an ingress controller a Jenkins controller still provisions, connects, and builds; it is reachable through its in-cluster Service with `kubectl port-forward`, and it reports a `NoExternalURL` condition saying so. That is the whole difference, and it is why the tutorial needs no domain.

For production:

- Varroa creates one Ingress for the dashboard and one per Jenkins controller.
- The dashboard host **must** have valid TLS: the BFF sets `Secure` cookies on login, and browsers silently drop those over plain HTTP, which makes login impossible.
- You need one hostname for the dashboard (e.g. `app.example.com`), plus per-controller hostnames (`<controller>.example.com`) unless you use path-mode ingress, which serves every controller under one host and removes the wildcard-DNS requirement. [external-dns](https://github.com/kubernetes-sigs/external-dns) automates the records nicely. See [Ingress](ingress.md).

## Identity provider (pick one)

Evaluating: use local auth and skip this section. Production: pick a real provider — local auth stores credentials as `User` CRs and has no account recovery story.

| You have | Use | Notes |
|---|---|---|
| An OIDC provider (Auth0, Okta, Azure AD, Keycloak, Google, …) | Direct OIDC | No Dex needed |
| GitHub OAuth, SAML, or an LDAP you want brokered | Dex (bundled, optional) | Dex federates it to OIDC |
| An LDAP directory, no broker wanted | Native LDAP mode | BFF binds directly |
| Nothing yet / evaluating | Local auth | Users stored as `User` CRs |

Details and trade-offs in [Authentication](../security/authentication.md).

## Tools

| Tool | Version | Purpose |
|---|---|---|
| `kubectl` | matching your cluster | Manage cluster resources |
| `helm` | 3.x | Install the Varroa chart |
| `git` | any recent | Bundle source repositories |
| `jq` | any recent | Parse API responses in the how-tos |

Nice to have: [`kind`](https://kind.sigs.k8s.io/) for a local test cluster (`make localdev` stands up the whole stack on one — see [Local development](local-development.md)), [`k9s`](https://k9scli.io/) for interactive cluster inspection, [`stern`](https://github.com/stern/stern) for multi-pod log tailing (handy across operator/gateway/bff at once).

## Sizing

The control plane itself is light (defaults total well under 2 CPU / 4 GiB across all components). Capacity planning is dominated by the Jenkins controllers you'll run: budget per controller whatever you'd give a standalone Jenkins of that team's size (the shipped presets start at 1 CPU / 2 GiB / 10 GiB). See [Scaling](../architecture/scaling.md).

## Network

If you enable the chart's [network policies](network-policies.md), your CNI must enforce `NetworkPolicy` (Calico, Cilium, etc. — the default kindnet in `kind` does not).

## Verify

```bash
kubectl version --short              # server ≥ 1.30
kubectl get ingressclass             # at least one class present
kubectl get storageclass             # one marked (default)
helm version --short                 # v3.x
```

**Verify:** all four commands succeed and show the expected minimums before continuing to [Helm install](helm-install.md).

## Related pages

- [Helm install](helm-install.md) — the next step
- [Ingress](ingress.md) — hostname and TLS layout
- [Authentication](../security/authentication.md) — choosing an auth mode
