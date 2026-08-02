# Tutorial: Authoring a Bundle

<!-- sources: docs/config/bundle-sources.md, docs/config/composed-bundles.md, docs/config/items.md — golden-path composition -->

[Your first controller](first-controller.md) got a Jenkins running on Varroa's built-in starter bundle. This page replaces that starter with configuration you own.

Start from a `Connected` controller. Everything here is additive — you are changing what a controller is configured with, not how it is deployed.

## What you are replacing

The starter bundle is two [catalog items](../config/casc-catalog.md) in the operator's namespace, composed by a `ComposedBundle` named `varroa-starter`:

```bash
kubectl get catalogitem -n varroa -l varroa.dev/starter=true
kubectl get composedbundle varroa-starter -n varroa -o yaml
```

Read them if you want a worked example. Do not edit them: the operator reconciles them back from content embedded in its own binary every minute. Point your controller at your own bundle instead.

## 1. Write the bundle

A [CloudBees-style bundle](../config/bundle-sources.md) is a directory in a git repository:

```
bundles/demo/
├── bundle.yaml
├── jenkins.yaml
└── items.yaml
```

```yaml
# bundle.yaml
id: demo
version: "1"
apiVersion: "2"
jcasc: [jenkins.yaml]
items: [items.yaml]
```

```yaml
# jenkins.yaml
jenkins:
  systemMessage: "Managed by Varroa — ${varroa_controller_name}"
```

```yaml
# items.yaml
items:
  - kind: pipeline
    name: hello
    definition:
      script: |
        pipeline { agent any; stages { stage('hello') { steps { echo 'hello from varroa' } } } }
```

`${varroa_controller_name}` is [injected automatically](../config/composed-bundles.md#injected-variables). Any `${...}` placeholder that Varroa cannot resolve blocks provisioning by design, so a typo fails loudly instead of reaching Jenkins as a literal.

Two things to leave out, because Varroa owns them and will strip or override anything you write:

- `authorizationStrategy` — [Jenkins RBAC](../security/jenkins-rbac.md) is generated from `JenkinsRole` and `JenkinsRoleBinding` CRDs.
- `unclassified.location.url` — derived from the controller's resolved ingress host.

Plugin versions are worth thinking about before you pin any. The [version profile](../config/jenkins-versions.md) supplies a locked plugin set matched to the Jenkins core, and a version you pin that disagrees with that lock is rejected rather than silently resolved — see [plugin pinning](../config/plugin-pinning.md) for how the two interact.

## 2. Compose it

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ComposedBundle
metadata: { name: demo, namespace: varroa }
spec:
  inputs:
    - gitSource:
        repoURL: https://github.com/<you>/casc-bundles.git
        path: bundles/demo
        revision: main
```

```bash
kubectl apply -f bundle.yaml
```

**Verify:** `kubectl get composedbundle demo -n varroa -o jsonpath='{.status.phase}'` → `Ready` ([troubleshooting](../config/bundle-sources.md#troubleshooting) if `Invalid`).

`inputs` is an ordered list, and a bundle may mix sources: a git repository, an [OCI artifact](../config/bundle-sources.md), and catalog items published by a platform team. Later inputs merge over earlier ones, which is how a shared baseline and per-team overrides live in one bundle. See [composed bundles](../config/composed-bundles.md).

## 3. Point the controller at it

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata: { name: demo, namespace: varroa }
spec:
  namespace: varroa
  composedBundleRef: { name: demo }
```

```bash
kubectl apply -f controller.yaml
```

The controller re-resolves its bundle, applies the new configuration through the [mite](../architecture/mite.md), and restarts only if the change requires it (a new plugin, or a JCasC key that Jenkins cannot reload live).

**Verify:**

- The system message reads "Managed by Varroa — demo".
- The `hello` pipeline exists. Whether the starter's `hello-varroa` pipeline is also removed is decided by your bundle's `itemRemoveStrategy` — see [items](../config/items.md).
- `kubectl get controller demo -n varroa -o jsonpath='{.status.phase}'` says `Connected`.

That is the whole loop: git → composed bundle → controller → converged Jenkins.

## Where to go next

- Publish reusable fragments so teams compose instead of copy: [catalog items](../config/casc-catalog.md).
- Pin a Jenkins version and its plugin set: [Jenkins versions](../config/jenkins-versions.md) and [plugin pinning](../config/plugin-pinning.md).
- Roll a bundle change across a brood in stages: [rollout waves](../operations/rollout-waves.md).
