# Author a Custom Bundle

Replace the starter configuration on the `demo` controller with a Git-backed bundle.

## Create the Bundle

Add these files to a Git repository:

```text
bundles/demo/
├── bundle.yaml
├── jenkins.yaml
└── items.yaml
```

`bundle.yaml`:

```yaml
id: demo
version: "1"
apiVersion: "2"
jcasc:
  - jenkins.yaml
items:
  - items.yaml
```

`jenkins.yaml`:

```yaml
jenkins:
  systemMessage: "Managed by Varroa: ${varroa_controller_name}"
```

`items.yaml`:

```yaml
items:
  - kind: pipeline
    name: hello
    definition:
      script: |
        pipeline {
          agent any
          stages {
            stage('Hello') {
              steps { echo 'hello from Varroa' }
            }
          }
        }
```

Varroa resolves injected controller variables before applying the bundle. Any unresolved Varroa variable blocks provisioning. Jenkins secret-source expressions remain available for Jenkins to resolve.

Do not set `jenkins.authorizationStrategy` or `unclassified.location.url`. Varroa owns Jenkins authorization and derives the controller URL from routing configuration. Use version profiles for compatible Jenkins core and plugin versions.

Commit and push the files. Prefer an immutable tag or commit for production bundles.

## Create a ComposedBundle

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ComposedBundle
metadata:
  name: demo
  namespace: jenkins-demo
spec:
  inputs:
    - gitSource:
        repoURL: https://github.com/example/casc-bundles.git
        path: bundles/demo
        revision: main
```

```bash
kubectl apply -f composed-bundle.yaml
kubectl get composedbundle demo -n jenkins-demo -w
```

Wait for `status.phase` to become `Ready`. A private Git repository requires `gitSource.secretRef` naming a Secret in `jenkins-demo`. See [Bundle sources](../config/bundle-sources.md).

Each input must set exactly one of `itemRef`, `gitSource`, or `ociSource`. Inputs merge in order. The default `jcascMergeStrategy` rejects conflicting keys; set it to `override` only when later inputs should replace earlier values.

## Attach the Bundle

```bash
kubectl patch controller demo -n jenkins-demo --type=merge \
  -p '{"spec":{"composedBundleRef":{"name":"demo"}}}'
kubectl get controller demo -n jenkins-demo -w
```

Reloadable JCasC and item changes apply through the mite. Plugin, image, and other restart-class changes can roll the Jenkins pod according to its reconciliation policy.

Verify that the controller returns to `Connected`, the system message names `demo`, and the `hello` pipeline runs. Item removal follows the bundle's item removal strategy. See [Managed items](../config/items.md).

Continue with [Composed bundles](../config/composed-bundles.md), [Catalog items](../config/casc-catalog.md), or [Plugin pinning](../config/plugin-pinning.md).
