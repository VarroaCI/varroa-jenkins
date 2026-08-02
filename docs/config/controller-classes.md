# Controller classes

`ControllerClass` is a cluster-scoped CRD that layers cluster-level defaults between
`ProvisioningDefaults` and each Controller's own spec. Think of it as a
**GatewayClass or StorageClass for Jenkins controllers** — a named template that
platform administrators define once and controller owners opt into via
`spec.className`.

## When to use

- **Environment templates** — define a `prod-large` class with generous resources,
  a `dev-small` class with minimal resources, and let team members select the
  right tier when creating a controller.
- **Centralised pod-scheduling policy** — enforce node selectors, tolerations,
  affinity rules, or security contexts across a whole class of controllers
  without editing each Controller individually.
- **Mite image rollout** — update the mite sidecar version at the class level,
  or set it per-Controller via `spec.miteSpec.image` (which wins over the class).
  The change converges live on already-Connected controllers
  (see [Per-field liveness](#per-field-liveness) below).

## Full field list

| Field | Type | Description |
|-------|------|-------------|
| `nodeSelector` | `map[string]string` | Pod node selector |
| `tolerations` | `[]corev1.Toleration` | Pod tolerations |
| `affinity` | `*corev1.Affinity` | Pod scheduling affinity |
| `securityContext` | `*corev1.PodSecurityContext` | Pod-level security context |
| `podLabels` | `map[string]string` | Labels merged onto the controller pod template |
| `podAnnotations` | `map[string]string` | Annotations merged onto the controller pod template |
| `ingressClassName` | `string` | Default ingress class name |
| `ingressAnnotations` | `map[string]string` | Annotations merged onto the controller ingress |
| `resources` | `*corev1.ResourceRequirements` | CPU/memory for the Jenkins container |
| `persistence` | `*PersistenceSpec` | PVC size/storage class |
| `probes` | `*ProbesSpec` | Health probe tuning |
| `mite` | `*ClassMiteSpec` | Mite sidecar image and pull policy |
| `imagePullSecrets` | `[]string` | Image-pull secret names |
| `jvmOpts` | `string` | JVM options prepended to `JAVA_OPTS` |

## Precedence chain

The effective value for every Controller field is resolved by the operator at
provisioning time in this order (later wins):

1. **Operator default** — baked-in or `ProvisioningDefaults`
2. **`ControllerClass`** (selected by `spec.className`)
3. **Controller's own spec** — always wins per-field

### Mite image / pull policy precedence

The mite sidecar image and pull policy follow a **four-tier** chain specific to
those two fields (distinct from the general three-tier chain above because
`resourceOverlay` can reach directly into the StatefulSet pod template):

| Priority | Source | Description |
|----------|--------|-------------|
| 1 (highest) | `spec.resourceOverlay.statefulSet` | An explicit `image` or `imagePullPolicy` on the `mite` container in the raw StatefulSet overlay. This is the Tier-3 escape hatch (see [Pod customisation](pod-customization.md)). |
| 2 | `spec.miteSpec.image` / `spec.miteSpec.imagePullPolicy` | Set directly on the Controller. Wins over the class and the operator default. |
| 3 | `ControllerClass` `spec.mite.image` / `spec.mite.imagePullPolicy` | The resolved class (when configured and its mite block is non-empty). Wins over the operator default. |
| 4 (lowest) | Operator-wide default | Image: `VARROA_MITE_IMAGE` env var (or baked-in fallback `ghcr.io/varroaci/varroa-jenkins:main` when unset). The Helm chart sets this env var from `operator.miteImage`, which defaults to `operator.image.repository:operator.image.tag` — so a fleet-wide tag bump moves mites along with the control plane unless `operator.miteImage` is set explicitly. Pull policy: the k8s-mirroring default computed from the resolved image — `:latest` / untagged → `Always`; otherwise `IfNotPresent`. |

Example: a Controller overrides the class mite image while keeping the class
pull policy:

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: team-ci
  namespace: varroa
spec:
  className: prod-large                  # class sets spec.mite.image + imagePullPolicy
  miteSpec:
    image: ghcr.io/varroaci/varroa-jenkins:v2.1   # wins over class image
    # imagePullPolicy is unset here — the class's value still applies
```

Editing `spec.miteSpec.image` or `spec.miteSpec.imagePullPolicy` on a Running
or Connected controller triggers **one rolling restart** of that controller's
mite and init containers via the existing container-spec-roll mechanism. The
roll is scoped to the edited Controller only — it does not affect other
controllers in the same class.

### Per-field liveness

> ⚠️ **This is a critical operational nuance. Read carefully.**

Not all ControllerClass fields converge on an already-running (Connected)
controller at the same time. The operator distinguishes two groups:

| Converges **live** on an already-Connected Controller | Takes effect **only at the next provisioning** |
|-------------------------------------------------------|------------------------------------------------|
| `spec.miteSpec.image` / `spec.miteSpec.imagePullPolicy` | `resources` |
| `mite.image` / `mite.imagePullPolicy` (from class) | `persistence` |
| `ingressClassName` | `probes` |
| `ingressAnnotations` | `imagePullSecrets` |
| | `nodeSelector` |
| | `tolerations` |
| | `affinity` |
| | `securityContext` |
| | `podLabels` |
| | `podAnnotations` |
| | `jvmOpts` |

The live-converging fields are checked on every reconcile tick; if the
class-derived value differs from what the StatefulSet currently has, the
operator transitions the controller to Provisioning and rolls the change.

The provisioning-only fields are applied only when the controller next enters
the Provisioning phase (for example, after a version change, plugin roll,
mite-image change, or manual reprovision). Changing these fields in a
ControllerClass does **not** immediately trigger a roll for an already-Connected
controller.

## Dangling `className` / `ClassResolved` fail-closed behaviour

When a Controller sets `spec.className` to a name that does not match any
existing ControllerClass (or the class object is temporarily unreadable), the
operator **fails closed**:

- `handleProvisioning` blocks — no StatefulSet or ConfigMap is created.
- `reconcileContainerSpecRoll` does not initiate a roll.
- **`reconcileIngress` is NOT blocked** — ingress TLS, host, and annotations
  still converge. This means a controller whose class becomes dangling keeps its
  ingress up to date, even though provisioning and container-roll operations
  are paused.

The condition `ClassResolved` surfaces the state:
- `True` / `ClassResolved` — class resolved successfully.
- `True` / `NoClassConfigured` — `className` is unset (no class layer applied).
- `False` / `ClassNotFound` — `className` is set but the class was not found;
  the controller is blocked until the name is corrected or the class is created.

## Worked example: whole-value-override surprise

`ControllerClassSpec` uses **whole-value override** for struct/object fields,
not field-level merge. This differs from what many administrators expect.

### Example: `securityContext`

**Class defines:**
```yaml
spec:
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
```

**Controller spec defines:**
```yaml
spec:
  securityContext:
    fsGroup: 2000
```

**Result:** The controller's `securityContext` becomes `{fsGroup: 2000}` only.
`runAsUser` and `runAsGroup` from the class are **lost** — the Controller's
spec replaces the entire struct, not individual fields.

### Example: `tolerations`

**Class defines:**
```yaml
spec:
  tolerations:
    - key: "dedicated"
      operator: "Equal"
      value: "jenkins"
      effect: "NoSchedule"
```

**Controller spec defines:**
```yaml
spec:
  tolerations:
    - key: "spot"
      operator: "Exists"
      effect: "NoSchedule"
```

**Result:** The controller runs with only the `spot` toleration. The class'
`dedicated` toleration is **not merged in** — whole-list override.

> 💡 **Tip:** To avoid surprises, treat the Controller's `tolerations` and
> `securityContext` fields as "I want exactly this, not this in addition to the
> class defaults." If you need additive behaviour, use the nodeSelector,
> podLabels, and podAnnotations fields instead — those are **key-level merged**
> (class and spec values are combined, with the Controller's values winning on
> key conflict).

## Example ControllerClass

See [`examples/controller-class.yaml`](../../examples/controller-class.yaml) for
a complete "prod-large" example.
