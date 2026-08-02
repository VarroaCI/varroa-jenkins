# Tutorial: Your First Controller

<!-- sources: docs/install/*, docs/config/*, docs/security/*, api/v1alpha1/types.go — golden-path composition of the handbook's install/config pages -->

From an empty cluster to a running, managed Jenkins in three steps. Each step links to the page that explains it in depth — this tutorial is the shortest correct path, not the reference.

You do not need a git repository, a bundle, or a domain name to get through this page. Varroa ships a built-in **starter bundle**, and a Controller that names no bundle uses it. Authoring your own configuration is the [next tutorial](custom-bundle.md).

## Prerequisites

The **evaluate** column of [Prerequisites](../install/prerequisites.md): a 1.30+ cluster with a default StorageClass, and `kubectl`. Ingress, TLS, DNS, and an identity provider are all production concerns — this tutorial reaches Jenkins with `kubectl port-forward`.

## 1. Install Varroa

Follow [Helm install](../install/helm-install.md):

```bash
helm dependency update charts/varroa
helm install varroa charts/varroa -n varroa --create-namespace
```

**Verify:** all pods Ready (`kubectl get pods -n varroa`), and the operator has seeded the starter bundle:

```bash
kubectl get composedbundle varroa-starter -n varroa
```

It reaches `Ready` within a minute of the operator starting.

## 2. Give yourself access

Bind your identity to the `operator` role ([Varroa RBAC](../security/varroa-rbac.md)). Nothing in the UI is useful until you do — Varroa denies by default, including to the person who installed it.

```yaml
apiVersion: varroa.dev/v1alpha1
kind: VarroaRoleBinding
metadata: { name: bootstrap-operators }
spec:
  roleRef: operator
  subjects: [{ kind: Group, name: "acme:platform-team" }]
```

```bash
kubectl apply -f binding.yaml
```

**Verify:** after re-login, the dashboard shows the create-controller button enabled.

## 3. Create a controller

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata: { name: demo, namespace: varroa }
spec:
  namespace: varroa
```

```bash
kubectl apply -f controller.yaml
kubectl get controller demo -n varroa -w
```

That is the whole spec. Two fields are doing work by omission:

- **No `composedBundleRef`** — the controller uses the built-in starter bundle: a system message, a Kubernetes cloud so builds get agents, and one sample pipeline.
- **No `version`** — the controller runs the Jenkins core that matches Varroa's embedded plugin lock, so core and plugins agree by construction. Pin a version when you want to control upgrades; see [Jenkins versions](../config/jenkins-versions.md).

You'll see the [phase walk](../architecture/overview.md#concepts-controller-lifecycle): `Pending → Provisioning` (StatefulSet, RBAC built) `→ Running` (Jenkins booting) `→ Connected` (the [mite](../architecture/mite.md) registered and streaming). First boot pulls images and installs the pinned plugin set — a few minutes is normal.

(The dashboard's creation wizard does this same step with version/size cards — use whichever you prefer. In a multi-cluster installation, the wizard also asks you to pick a target cluster on the Basics step.)

**Verify:** phase is `Connected`.

## 4. Look at it

With no ingress host configured, reach Jenkins through the Service:

Varroa names a controller's Kubernetes objects `<controller>-<first 8 characters of its UID>`, so that deleting and recreating a controller never collides with the old one's PersistentVolumes:

```bash
PREFIX=demo-$(kubectl get controller demo -n varroa -o jsonpath='{.metadata.uid}' | cut -c1-8)
kubectl port-forward -n varroa "svc/$PREFIX-svc" 8080:8080
```

Then open <http://localhost:8080>.

**Verify, end to end:**

- The system message says the controller is running the built-in starter bundle.
- The `hello-varroa` pipeline job exists — run it; it prints `hello from Varroa`.
- `kubectl get controller demo -n varroa -o jsonpath='{.status.phase}'` still says `Connected`.

The controller also reports why it has no URL:

```bash
kubectl get controller demo -n varroa -o jsonpath='{.status.conditions[?(@.type=="NoExternalURL")].message}'
```

That condition is informational. To give the controller a real hostname, set `ProvisioningDefaults.rootDomain` (every controller gets `<name>.<rootDomain>`) or `spec.ingressSpec.host` on this one — see [Ingress and routing](../install/ingress.md).

## Where to go next

- Replace the starter bundle with your own configuration: [Authoring a bundle](custom-bundle.md).
- Turn this evaluation install into a production one: the **production** column of [Prerequisites](../install/prerequisites.md).
- Split platform baseline from team config with [catalog items](../config/casc-catalog.md) and input ordering.
- Put your brood behind a canary with [rollout waves](../operations/rollout-waves.md).
- Tighten who can do what: [Varroa RBAC](../security/varroa-rbac.md) and [Jenkins RBAC](../security/jenkins-rbac.md).
- Lock the network down with [network policies](../install/network-policies.md).
- Set up [observability](../operations/observability.md) before you need [troubleshooting](../operations/troubleshooting.md).
