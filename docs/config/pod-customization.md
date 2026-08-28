# Customize controller pods

Use `spec.podOverrides` for supported pod settings. Use
`spec.resourceOverlay` only when no typed field can express the required
StatefulSet, Service, or Ingress patch.

## Apply typed overrides

```yaml
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: demo
  namespace: teams-platform
spec:
  podOverrides:
    jvmOpts: -XX:MaxRAMPercentage=75 -Duser.timezone=UTC
    env:
      - name: HTTP_PROXY
        value: http://proxy.example.com:3128
    volumes:
      - name: ca-bundle
        configMap:
          name: organization-ca
    volumeMounts:
      - name: ca-bundle
        mountPath: /etc/organization-ca
        readOnly: true
    nodeSelector:
      workload: jenkins
```

| Field | Target |
|---|---|
| `env`, `envFrom` | Jenkins container |
| `volumes` | Pod |
| `volumeMounts` | Jenkins container |
| `podLabels`, `podAnnotations` | Pod template metadata |
| `labels`, `annotations` | StatefulSet metadata |
| `jvmOpts` | Jenkins Java options |
| `nodeSelector`, `tolerations`, `affinity` | Scheduling |
| `securityContext` | Pod security context |

Environment variables, volumes, and mounts merge by name. Prefer controller
fields such as `resources`, `persistence`, `ingressSpec`, and `probes` when
available.

## Tune health probes

`spec.probes` controls startup, readiness, and liveness timing. Set
`disabled: true` to omit one probe.

| Probe | Initial delay | Period | Timeout | Failure threshold |
|---|---:|---:|---:|---:|
| Startup | 10s | 10s | 5s | 30 |
| Readiness | 0s | 10s | 5s | 3 |
| Liveness | 0s | 10s | 5s | 6 |

Timing fields have CRD bounds. Kubernetes requires `successThreshold: 1` for
startup and liveness probes.

## Apply a raw overlay

Each value is strategic-merge-patch YAML:

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
                  limits:
                    ephemeral-storage: 4Gi
    service: |
      metadata:
        annotations:
          service.example.com/internal: "true"
```

Overlays apply last and can replace managed fields. Conflicts appear in
`status.overlayWarnings`; `OverlayActive` reports that an overlay is present.
Changes to immutable StatefulSet fields require recreation.

## Preview and troubleshoot

Use the controller preview endpoint before saving an overlay. It returns merged
YAML, diffs, warnings, and the selected baseline. Preview requires controller
update permission, or create permission for a new controller, and is available
only for the local cluster.

| Symptom | Check |
|---|---|
| Override does not persist | `status.overlayWarnings` and typed alternatives |
| Pod is unschedulable | Pod events and effective scheduling fields |
| Overlay is rejected | YAML syntax and immutable fields |
| Write reports an ownership conflict | [Canonical field ownership procedure](../agents/writing.md#manage-field-ownership) |
