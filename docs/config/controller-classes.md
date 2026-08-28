# Apply controller classes

A cluster-scoped `ControllerClass` supplies reusable infrastructure defaults to
controllers that select it with `spec.className`.

## Create a class

```yaml
apiVersion: varroa.dev/v1alpha1
kind: ControllerClass
metadata:
  name: production
spec:
  nodeSelector:
    workload: jenkins
  tolerations:
    - key: workload
      operator: Equal
      value: jenkins
      effect: NoSchedule
  resources:
    requests:
      cpu: "1"
      memory: 2Gi
    limits:
      memory: 4Gi
  persistence:
    size: 100Gi
  ingressClassName: nginx
  podLabels:
    environment: production
  jvmOpts: -XX:MaxRAMPercentage=75
  mite:
    image: ghcr.io/varroaci/varroa-jenkins:latest
    imagePullPolicy: IfNotPresent
```

| Area | Fields |
|---|---|
| Scheduling | `nodeSelector`, `tolerations`, `affinity` |
| Security and metadata | `securityContext`, `podLabels`, `podAnnotations` |
| Jenkins container | `resources`, `jvmOpts` |
| Storage and health | `persistence`, `probes` |
| Images | `imagePullSecrets`, `mite.image`, `mite.imagePullPolicy` |
| Ingress | `ingressClassName`, `ingressAnnotations` |

## Select and override

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: team-ci
  namespace: teams-platform
spec:
  className: production
```

Controller fields override class fields. Maps such as labels and annotations
merge by key. Lists and structs such as tolerations and `securityContext` are
whole-value overrides.

## Apply changes safely

Mite image and pull-policy changes can trigger a container roll. Ingress class
and annotations reconcile independently. Other class values take effect during
controller provisioning or reprovisioning.

Kubernetes prevents updates to immutable StatefulSet fields such as volume
claim templates. Recreate the controller when an immutable storage change is
required, and preserve data according to your storage policy.

If a selected class is missing, provisioning and container rolls stop.
`ClassResolved=False` reports reason `ClassNotFound`. Create the class or clear
`spec.className`.

Use [pod customization](pod-customization.md) for per-controller overrides.
