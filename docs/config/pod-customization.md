# Pod Customization

<!-- sources: api/v1alpha1/types.go (PodOverrides, ResourceOverlay, ControllerConditionType), internal/overlay/, internal/api/handlers.go (POST /controllers/{ns}/{name}/preview), internal/controller/controller_controller.go (overlayActiveCondition) -->

When a controller needs more than the standard StatefulSet — extra env vars, volumes, node placement, JVM flags, or a change Varroa's typed fields don't model — use `podOverrides` (typed, safe, for pod-level needs) or `resourceOverlay` (raw strategic-merge patches, admin-only, for everything else).

## Concepts

- **`spec.podOverrides`** — typed, pod-scoped fields merged onto the Jenkins StatefulSet. List fields merge **by key** (env by name, volumes by name), so you add/override entries without replacing whole lists.
- **`spec.resourceOverlay`** — raw strategic-merge-patch YAML applied to the generated **StatefulSet**, **Service**, or **Ingress**. The escape hatch: full power, admin-gated.
- **Warn-but-allow guardrails**: overlays that touch fields Varroa itself manages produce warnings surfaced in `status.overlayWarnings` — the change still applies, but you're told you're fighting the operator (and reconciliation may re-assert managed fields).
- **Merge preview**: the API can compute the merged result against the live object *without saving*, so you see exactly what an overlay will do before committing. The dashboard's controller editor shows this preview inline.

## The pod/resource layering chain

A Controller's final provisioned StatefulSet, Service, and Ingress are produced by five stages,
arranged in two liveness classes:

| # | Layer | Kind | Live-converged? |
|---|---|---|---|
| 1 | **Creation-time seeding** — `ProvisioningDefaults` (CPU/memory/storage/rootDomain). Read once by the BFF at controller-render time (`POST /controllers/{ns}/render`); editing `ProvisioningDefaults` after creation has no effect on an existing Controller. | upstream default | No |
| 2 | **Class-defaults** — `ControllerClass` (once the `ControllerClass` CRD ships). Continuously reconciled and layered onto every Controller that sets `spec.className`. | upstream default | No |
| 3 | **Tier 1** — the Controller's own typed spec. Curated, validated, UI-form-rendered fields (`resources`, `persistence`, `ingressSpec`, `probes`, etc.). | operator-spec layer | Yes (Tiers 1–3) |
| 4 | **Tier 2** — `podOverrides`. Typed corev1 passthrough; list fields (`env`, `volumes`, `volumeMounts`) merge by key, not wholesale replacement. | operator-spec layer | Yes (Tiers 1–3) |
| 5 | **Tier 3** — `resourceOverlay`. Raw strategic-merge-patch YAML, applied last. The escape hatch — "unsupported shape" territory. Watch for the `OverlayActive` status condition and `status.overlayWarnings`. | operator-spec layer | Yes (Tiers 1–3) |

Tiers 1–3 (the operator-spec layers) are continuously converged: the operator's drift-detection
reprovision path detects a diff and re-applies desired state, though not necessarily on the same
tick it was written. The two upstream layers (1–2) do **not** share that guarantee — they seed
or default once and are not themselves live-reconciled targets. Do not present all five stages as
a single uniform live-precedence table; only the bottom three are part of the running
reconciliation loop.

> **For the other chain** (JCasC/bundle composition), see [Composed bundles](composed-bundles.md).

### The OverlayActive condition

The operator sets a `OverlayActive` condition on every reconcile (outside the provisioning path)
so you can tell at a glance whether a controller is using the Tier-3 escape hatch:

| Status | Reason | Message |
|---|---|---|
| `True` | `ResourceOverlaySet` | Comma-joined list of overlay keys in field order — `statefulSet`, `service`, `ingress` (e.g. `"statefulSet, ingress"`). |
| `False` | `NoResourceOverlay` | `"no resource overlay configured"` |

```bash
kubectl get controller <name> -o jsonpath='{.status.conditions[?(@.type=="OverlayActive")]}'
```

## Reference: podOverrides fields

| Field | Merges onto | Notes |
|---|---|---|
| `env` / `envFrom` | jenkins container | `env` merged by name |
| `volumes` / `volumeMounts` | pod / jenkins container | merged by name |
| `podLabels` / `podAnnotations` | pod template metadata | |
| `labels` / `annotations` | StatefulSet metadata | |
| `jvmOpts` | `JAVA_OPTS` | appended (space-joined) to the baseline value |
| `nodeSelector` / `tolerations` / `affinity` | pod spec | |
| `securityContext` | pod spec | pod-level security context |

## Health probes

`spec.probes` is the only Controller-spec field for probe timing and enable/disable. It renders curated Jenkins container probes for `startupProbe`, `readinessProbe`, and `livenessProbe`. When omitted, Varroa installs all three with these defaults:

| Probe | Defaults |
|---|---|
| Startup | `initialDelaySeconds: 10`, `periodSeconds: 10`, `timeoutSeconds: 5`, `failureThreshold: 30`, `successThreshold: 1` |
| Readiness | `initialDelaySeconds: 0`, `periodSeconds: 10`, `timeoutSeconds: 5`, `failureThreshold: 3`, `successThreshold: 1` |
| Liveness | `initialDelaySeconds: 0`, `periodSeconds: 10`, `timeoutSeconds: 5`, `failureThreshold: 6`, `successThreshold: 1` |

The startup defaults give Jenkins about 310 seconds to boot (`10s` initial delay + `30 x 10s` retries) before the pod is marked failed.

Tuning notes:

- `disabled: true` omits just that probe.
- `initialDelaySeconds`, `periodSeconds`, `timeoutSeconds`, `failureThreshold`, and `successThreshold` are all optional and override only the fields you set.
- Readiness is what makes `PodReady` mean "Jenkins is actually serving"; disabling readiness re-opens the old PodReady-versus-running gap.
- Keep `startupProbe.successThreshold` and `livenessProbe.successThreshold` at `1`; Kubernetes rejects larger values for those probe kinds.
- A genuinely custom probe handler (different `httpGet` path, `exec`, `tcpSocket`, different port) is not expressible via any typed field. Use `spec.resourceOverlay.statefulSet` as a raw strategic-merge patch instead (see example below). Note the same "unsupported shape, no drift/convergence guarantee" framing applies — Varroa will re-assert its `spec.probes` output on every reconcile, so this is a last resort.

### Custom probe handler via resourceOverlay

To replace the Jenkins container's `livenessProbe` with a custom `exec` probe (e.g. a script that checks a health endpoint):

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata: { name: demo, namespace: teams-platform }
spec:
  namespace: teams-platform
  version: "2.555"
  composedBundleRef: { name: platform-baseline }
  resourceOverlay:
    statefulSet: |
      spec:
        template:
          spec:
            containers:
              - name: jenkins
                livenessProbe:
                  exec:
                    command:
                      - /bin/sh
                      - -c
                      - curl -sf http://localhost:8080/health
                  initialDelaySeconds: 15
                  periodSeconds: 20
```

## How to use podOverrides

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata: { name: demo, namespace: teams-platform }
spec:
  namespace: teams-platform
  version: "2.555"
  composedBundleRef: { name: platform-baseline }
  podOverrides:
    jvmOpts: "-XX:MaxRAMPercentage=75 -Duser.timezone=UTC"
    env:
      - name: HTTP_PROXY
        value: http://proxy.example.com:3128
    volumes:
      - name: ca-bundle
        configMap: { name: corp-ca-bundle }
    volumeMounts:
      - name: ca-bundle
        mountPath: /etc/corp-ca
        readOnly: true
    nodeSelector:
      workload: ci
    tolerations:
      - key: ci-dedicated
        operator: Exists
        effect: NoSchedule
```

```bash
kubectl apply -f controller.yaml
```

**Verify:** after the roll, `kubectl get sts demo -n teams-platform -o jsonpath='{.spec.template.spec.containers[0].env}'` includes `HTTP_PROXY`, and `kubectl get pod -n teams-platform -l app=demo -o jsonpath='{.items[0].spec.nodeSelector}'` shows `workload: ci`.

## How to use resourceOverlay

Raw strategic-merge patches per resource. PVC sizing customization goes through the StatefulSet overlay's `volumeClaimTemplates` (there is no dedicated PVC key) — but note that `volumeClaimTemplates`, `serviceName`, `selector`, and `podManagementPolicy` are immutable in Kubernetes: the operator applies overlay changes to them only when the StatefulSet is first created, and preserves the live values on every later update. Changing them for an existing controller requires teardown/recreate:

```yaml
spec:
  resourceOverlay:
    statefulSet: |
      spec:
        template:
          spec:
            containers:
              - name: jenkins
                resources:
                  limits: { ephemeral-storage: 4Gi }
    service: |
      metadata:
        annotations:
          service.beta.kubernetes.io/aws-load-balancer-internal: "true"
    ingress: |
      metadata:
        annotations:
          nginx.ingress.kubernetes.io/whitelist-source-range: 10.0.0.0/8
```

Applying `resourceOverlay` requires admin-level Varroa access ([Varroa RBAC](../security/varroa-rbac.md)).

**Verify:** `kubectl get controller demo -n teams-platform -o jsonpath='{.status.overlayWarnings}'` — empty means the overlay didn't collide with operator-managed fields; entries name the exact paths in tension.

## How to preview a merge before applying

```bash
curl -sf -X POST -H "Authorization: Bearer $VARROA_API_KEY" \
  https://app.example.com/api/v1/clusters/core/controllers/teams-platform/demo/preview \
  -d @- <<'EOF'
{"resourceOverlay": {"statefulSet": "spec:\n  template:\n    spec:\n      containers:\n        - name: jenkins\n          resources:\n            limits: {ephemeral-storage: 4Gi}\n"}}
EOF
```

The response contains the fully merged objects (computed against the live resources) and any guardrail warnings — nothing is saved. In the dashboard, the controller edit form shows the same live preview.

**Verify:** the previewed StatefulSet shows your change exactly where you expect it, with no unexpected warnings, before you `kubectl apply`.

> [!WARNING]
> Overlays are re-applied on every reconcile over the operator's generated resources. If a warning says you're overriding a Varroa-managed field (image, mite sidecar, bootstrap mounts), expect the operator and your overlay to fight — fix the intent at the right layer instead ([versions](jenkins-versions.md) for images, [ingress](../install/ingress.md) for hosts/TLS).

## Troubleshooting

- Override "didn't take" → check `status.overlayWarnings`; a managed field re-asserted by the operator wins.
- Pod unschedulable after overrides → your `nodeSelector`/`tolerations` don't match any node; the events on the pod say so.
- Preview 403 → `resourceOverlay` and preview require admin capability.

## The Spec Editor (dashboard UI)

The Configuration tab in the controller workspace ships a **Spec Editor** card that replaces both the old read-only CRD/YAML Diagnostics view and the former ad-hoc overlay editor. It groups every layer of `Controller.spec` into tiered tabs:

| Tier | What it edits | UI surface |
|---|---|---|
| **Tier 1 — Curated form** | The Controller's typed spec fields: `rbacSpec`, `pluginSpec`, `backupSpec`, `resources`, `persistence`, `className`. | Auto-generated form from the BFF's OpenAPI schema. Each field renders a type-appropriate widget (text, number, checkbox, select, object group). |
| **Tier 2 — `podOverrides` YAML** | `spec.podOverrides` — typed, pod-scoped customizations (env, volumes, node placement, JVM flags). | CodeMirror 6 YAML editor with live parse validation and AJV schema validation. |
| **Tier 2 — `ingressSpec` YAML** | `spec.ingressSpec`. Moved out of the generated form because `annotations` is a free-form key/value map the form rendered unusably. | CodeMirror 6 YAML editor, AJV-validated against the IngressSpec schema. |
| **Tier 2 — `miteSpec` YAML** | `spec.miteSpec` — the mite sidecar's `resources` (standard k8s `requests`/`limits`), `image`, and `imagePullPolicy`. Moved out of the generated form for the same reason. | CodeMirror 6 YAML editor, AJV-validated against the MiteSpec schema. |
| **Tier 3 — `resourceOverlay` YAML** | `spec.resourceOverlay.statefulSet`, `.service`, `.ingress` — raw strategic-merge patches. | Three CodeMirror 6 YAML editors in a sub-tab strip, no schema validation (arbitrary K8s patches are schema-less by nature). |

`spec.namespace` is rendered read-only: a controller's namespace is fixed at creation time and
the editor will not offer to change it.

If any YAML tier fails to parse, **Save spec** aborts and names the offending tier — the save is
not sent partially. Fix or clear the tier and save again.

### Precedence and liveness

Tiers 1–3 are all **operator-spec layers**: changes made via the Spec Editor write `Controller.spec` only (never `ProvisioningDefaults` or a `ControllerClass` object). They converge live on every reconcile tick — what you save takes effect within seconds on a `Connected` controller.

The two upstream layers — `ProvisioningDefaults` (creation-time seeding) and `ControllerClass` (class defaults) — are **not** part of this editor. Their `Default*` fields are a one-shot seed read at controller-create time; editing `ProvisioningDefaults` or `ControllerClass` after creation has no effect on an **already-created** controller. Only new controllers pick up changes to those upstream layers.

> **Rule of thumb:** if you want a field change to apply to a running controller right now, use the Spec Editor (Tiers 1–3), `kubectl patch`, or `varroactl patch`. Do not edit `ProvisioningDefaults` expecting it to retroactively update every controller — it won't.

### SSA field-ownership conflicts

When two users or automation tools edit the same field at the same time, the server's server-side apply (SSA) field-manager tracking detects the conflict and returns a **409 Conflict** response. The Spec Editor surfaces this as a **Conflict dialog** with two choices:

- **Reload latest** — discards your draft and re-fetches the controller from the server, picking up the other writer's change.
- **Override anyway** — re-sends your edit with `?force=true`, telling the server to take ownership of the conflicting fields despite the conflict.

Choose Reload when you want to see and preserve the other change; choose Override when you are sure your edit should win.

### Migration note

The old read-only CRD/YAML view under Diagnostics is removed. Raw structured editing now lives in **Configuration → Spec Editor**. The underlying BFF endpoint (`GET …/yaml`) and `varroactl get controller -o yaml` still work — only the in-browser tab view is gone.

## Related pages

- [Scaling](../architecture/scaling.md) — resources and size presets (the typed path for CPU/memory/storage, now structured as requests/limits + persistence)
- [Ingress](../install/ingress.md) — prefer `ingressSpec` over ingress overlays
- [Varroa RBAC](../security/varroa-rbac.md) — the admin gate on overlays
