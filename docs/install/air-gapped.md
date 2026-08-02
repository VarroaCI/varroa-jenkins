# Air-gapped installation

<!-- sources: charts/varroa/values.yaml (networkPolicy.*, updateCenter.*), cmd/varroactl/export.go, cmd/varroactl/import.go -->

Run the Varroa control plane on a cluster that has no outbound internet access. The same approach works for clusters with egress-firewall policies that restrict traffic to a small set of internal registries.

## Overview

An air-gapped install replaces every external network dependency with a private mirror:

| Dependency | Air-gap replacement |
|---|---|
| `updates.jenkins.io` (plugin metadata + HPI downloads) | In-cluster [Update Center](../operations/update-center.md) with pre-seeded plugin packs |
| OCI registry (`ghcr.io/varroaci/...`) | Private registry mirror |
| Plugin pack distribution | `varroactl export` → sneakernet → `varroactl import` |
| Git bundle clones | `ociRegistryEgress` or `gitEgress` pinned to internal mirror |

## Prerequisites

- A working [Helm install](helm-install.md) in a non-air-gapped environment first, to confirm the chart values are correct before locking down network access.
- Access to a connected environment with `varroactl` (or the ability to build the backend image) to produce seed plugin packs and bundle snapshots.
- A private OCI registry reachable from the air-gapped cluster, OR a mechanism to transfer `tar://` or `dir://` plugin packs (USB drive, internal file server).
- A Docker config Secret (`kubernetes.io/dockerconfigjson`) for the private registry, if images are pulled from it.

### Step 1: Mirror plugin packs while connected

On a machine that has internet access and the Varroa backend image, export the plugin sets your controllers need.

**Full profile export** (the default `jenkins-version-2-555` profile ships with the chart):

```bash
varroactl export plugins \
  --profile jenkins-version-2-555 \
  --to oci://registry.internal.example.com/varroa/plugin-pack:jenkins-version-2-555
```

**Constrained export** (a subset of plugins, for smaller seed files):

```bash
cat > /tmp/plugins.yaml <<EOF
plugins:
  - artifactId: structs
    version: 362.va_b_695ef4fdf9
  - artifactId: scm-api
    version: 728.vc30dcf7a_0df5
EOF

varroactl export plugins \
  --profile jenkins-version-2-555 \
  --plugins-file /tmp/plugins.yaml \
  --to oci://registry.internal.example.com/varroa/plugin-pack:minimal
```

**Export for sneakernet transfer** (tar archive):

```bash
varroactl export plugins \
  --profile jenkins-version-2-555 \
  --to tar:///tmp/seed-2-555.tar.gz
```

Transfer the `.tar.gz` file to the air-gapped cluster via USB, internal file server, or whatever mechanism your security policy allows.

**Private registry auth:** If your registry uses a self-signed TLS certificate, pass `--insecure`:

```bash
varroactl export plugins \
  --profile jenkins-version-2-555 \
  --to oci://registry.internal.example.com/varroa/plugin-pack:jenkins-version-2-555 \
  --insecure
```

### Step 2 (optional): Baseline deny-all-egress

Before installing the chart, apply a default-deny-all-egress NetworkPolicy so no pod can make external connections until explicitly allowed:

```bash
kubectl apply -f docs/install/examples/air-gapped/deny-all-egress.yaml
```

This creates two policies in the `varroa-system` namespace: one that blocks all egress and one that blocks all ingress not explicitly permitted. Once the chart is installed with `networkPolicy.enabled=true`, the chart's per-component policies layer on top of this base deny. See [examples/air-gapped/](examples/air-gapped/) for the full worked manifest.

### Step 3: Install the chart

Create a values file that enables the update center, disables pull-through (no upstream to fetch from), and sets egress toggles for the OCI registry mirror:

```yaml
# values-airgap.yaml
updateCenter:
  enabled: true
  pullThrough:
    enabled: false               # no internet → no pull-through
  storage:
    type: oci                    # or "local" and pre-populate the PVC
    oci:
      ref: registry.internal.example.com/varroa/updatecenter-store:latest
      existingSecret: registry-cred   # kubernetes.io/dockerconfigjson Secret
    local:
      size: 10Gi

networkPolicy:
  enabled: true
  # Operator OCI-registry egress: allows operator pod to reach the private
  # registry for plugin-pack import and catalog operations. Mode-independent
  # (renders in both full and hive mode).
  ociRegistryEgress:
    enabled: true
    cidrs:
      - 10.0.0.0/8               # pin to your private registry CIDR
    ports:
      - 443
  # Update-center pull-through egress: not needed here since
  # pullThrough.enabled is false, but if you enable pull-through point
  # this at an internal mirror of updates.jenkins.io.
  pullThroughEgress:
    enabled: false
  # Update-center OCI-registry egress: allows updatecenter pod to reach the
  # OCI-backed seed store. Rendered only when storage.type=oci.
  updateCenterRegistryEgress:
    enabled: true
    cidrs:
      - 10.0.0.0/8
    ports:
      - 443

global:
  imagePullSecrets:
    - registry-cred               # pull backend/frontend images from private registry
```

Install:

```bash
helm install varroa charts/varroa -n varroa-system --create-namespace \
  -f values-airgap.yaml \
  --set auth.oidc.clientSecret=<your-secret> \
  --set auth.dashboardUrl=https://app.example.com
```

**Verify the update center is ready:**

```bash
kubectl get updatecenter varroa-update-center -o jsonpath='{.status.conditions}'
# Look for StorageReady=True, SeedImported=True (once seeded)
```

If you are using `storage.type=local`, the chart's default `UpdateCenter` CR uses local PVC storage. Verify the PVC binds:

```bash
kubectl get pvc -n varroa-system -l app.kubernetes.io/component=varroa-updatecenter
```

### Step 4: Seed the update center

Once the update-center pod is running, import the plugin pack(s) exported in step 1.

**From a tar archive** (sneakernet):

Copy the seed tarball to a pod or a host path, then:

```bash
# Extract the token
export VARROACTL_UC_TOKEN=$(kubectl get secret -n varroa-system \
  varroa-updatecenter-import-token -o jsonpath='{.data.token}' | base64 -d)

# Port-forward to reach the uc:// endpoint
kubectl port-forward -n varroa-system svc/varroa-updatecenter 8080:8080 &

# Import
varroactl import \
  --from tar:///path/to/seed-2-555.tar.gz \
  --to uc://localhost:8080
```

**From a private OCI registry:**

```bash
export VARROACTL_UC_TOKEN=$(kubectl get secret ... )

varroactl import \
  --from oci://registry.internal.example.com/varroa/plugin-pack:jenkins-version-2-555 \
  --to uc://varroa-updatecenter.varroa-system.svc:8080
```

**From a local OCI layout directory** (pre-transferred via sneakernet):

```bash
varroactl import \
  --from dir:///path/to/pluginpack-fixture \
  --to uc://localhost:8080
```

**Verify the import succeeded:**

```bash
kubectl get updatecenter varroa-update-center -o jsonpath='{.status.conditions}'
# SeedImported should be True
# CoverageComplete should be True (all declared plugins are present)
# Ready should be True
```

If `SeedImported` is False, check the operator logs:

```bash
kubectl logs -n varroa-system deploy/varroa-varroa-operator | grep -i updatecenter
```

### Step 5: Provision controllers

Once the update center reports `Ready=True`, the operator begins provisioning controllers. The operator's `ProvisioningDefaults` will show `WaitingForUpdateCenter=False` when the update center is ready.

```bash
kubectl get provisioningdefaults -o yaml
```

Create a controller as usual — the operator resolves plugin dependencies against the update center instead of `updates.jenkins.io`:

```bash
kubectl apply -f - <<EOF
apiVersion: varroa.dev/v1alpha1
kind: Controller
metadata:
  name: my-jenkins
  namespace: team-ns
spec:
  version: 2.555
EOF
```

**Verify:**

```bash
kubectl get controller -A -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
# Should show Connected or Running
kubectl get updatecenter varroa-update-center -o jsonpath='{.status.phase}'
# Should be Ready
```

### Step 6: Upgrade plugin sets

When you add a new plugin to a version profile or bundle, the update center reports a gap:

```bash
kubectl get updatecenter varroa-update-center -o jsonpath='{.status.gaps}'
```

The operator sets `WaitingForUpdateCenter` on any controller that depends on a missing plugin. Re-seed the new plugin pack:

```bash
varroactl import \
  --from oci://registry.internal.example.com/varroa/plugin-pack:new-plugins \
  --to uc://varroa-updatecenter.varroa-system.svc:8080
```

Once the gap clears, controllers automatically proceed to `Connected`.

### Step 7: Automate re-seeding (optional)

A `CronJob` can periodically re-import a plugin pack from a seed directory, keeping the update center in sync with an offline mirror:

```bash
kubectl apply -f docs/install/examples/air-gapped/cronjob-seed.yaml
```

This runs `varroactl import` hourly, pulling from a host-path volume containing the latest seed tarball. The `uc://` import endpoint is idempotent — already-imported digests are silently skipped. See [examples/air-gapped/cronjob-seed.yaml](examples/air-gapped/cronjob-seed.yaml) for the full manifest.

## Private registry image mirror

If the air-gapped cluster also needs to pull the Varroa backend and frontend images from a private registry, create a `dockerconfigjson` Secret and reference it in values:

```bash
kubectl create secret docker-registry registry-cred \
  -n varroa-system \
  --docker-server=registry.internal.example.com \
  --docker-username=<user> \
  --docker-password=<password>
```

Then in your values:

```yaml
global:
  imagePullSecrets:
    - registry-cred
operator:
  image:
    repository: registry.internal.example.com/varroa-jenkins
gateway:
  image:
    repository: registry.internal.example.com/varroa-jenkins
bff:
  image:
    repository: registry.internal.example.com/varroa-jenkins
frontend:
  image:
    repository: registry.internal.example.com/varroa-frontend
```

### Images Jenkins pods pull

Varroa's own images are only half the list. Every Jenkins controller pod also pulls:

- the **Jenkins core image** — `jenkins/jenkins:<version>`, where the version comes from the controller's resolved `JenkinsVersionProfile` (or the embedded plugin-lock baseline when `spec.version` is unset);
- the **agent image** named by whatever cloud configuration your bundle applies.

If you use the built-in starter bundle, that agent image is `jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21` — a pinned tag precisely so you can mirror it. Check the current value before mirroring, since it is refreshed alongside the plugin lock:

```bash
grep image: bundles/starter/jenkins.yaml
```

A controller whose agent image cannot be pulled still reaches `Connected` — the mite talks to Jenkins over the in-cluster Service — and only fails when a build asks for an agent, which shows up as agent pods stuck in `ImagePullBackOff` rather than as a controller-level error.

For the OCI storage backend, reference the same private registry:

```yaml
updateCenter:
  storage:
    type: oci
    oci:
      ref: registry.internal.example.com/varroa/updatecenter-store:latest
      existingSecret: registry-cred
```

## Network egress toggles

The chart ships three egress toggle groups under `networkPolicy` for air-gapped environments:

| Toggle | Component | Default | Purpose |
|---|---|---|---|
| `ociRegistryEgress` | operator | `enabled: true` | Operator reaches the private OCI registry for plugin-pack import and catalog operations. Mode-independent (renders in both full and hive mode). |
| `pullThroughEgress` | updatecenter | `enabled: true` | Update center reaches `updates.jenkins.io` (or an internal mirror) for pull-through caching. Rendered only when `updateCenter.pullThrough.enabled` is also true. |
| `updateCenterRegistryEgress` | updatecenter | `enabled: true` | Update center reaches an external OCI registry when `storage.type=oci`. Rendered only when the storage type is `oci`. |

Each toggle has `cidrs` (default `["0.0.0.0/0"]`) and `ports` (default `[443]`). Pin the CIDR to your internal registry addresses in production:

```yaml
networkPolicy:
  ociRegistryEgress:
    cidrs:
      - 10.0.0.0/8
      - 172.16.0.0/12
    ports:
      - 443
```

## Troubleshooting

| Symptom | Likely cause | What to check |
|---|---|---|
| Controller stays `Pending` | Update center not seeded, or `WaitingForUpdateCenter` condition | `kubectl get updatecenter varroa-update-center -o yaml` — check `status.conditions` |
| `StorageReady` is `False` | PVC not bound (local) or OCI registry unreachable (oci) | `kubectl describe pvc -n varroa-system` / ping the registry |
| `SeedImported` is `False` | Import failed or `VARROACTL_UC_TOKEN` not set | Check operator logs: `kubectl logs deploy/varroa-varroa-operator \| grep -i updatecenter` |
| `CoverageComplete` is `False` | A plugin declared in a profile/bundle is absent from the store | Inspect `status.gaps` on the `UpdateCenter` CR |
| `varroactl import` exits with 401 | `VARROACTL_UC_TOKEN` is unset or wrong | Verify the Secret exists: `kubectl get secret varroa-updatecenter-import-token -n varroa-system`; the key is `token`. |
| `varroactl import` exits with non-zero | Network unreachable, or the `--from` source doesn't exist | Check DNS/firewall rules for the `--from` URI |
| `WaitingForUpdateCenter` on controller | The operator has detected gaps and is blocking provisioning | Re-seed the missing plugins and wait for the next reconcile |
| Chart install fails with `required` error on `auth.oidc.clientSecret` | The chart requires this value | Pass `--set auth.oidc.clientSecret=<value>` or set it in your values file |

## Plugin integrity

The air-gapped path does not weaken Jenkin's per-plugin integrity checks. The jenkins-plugin-cli's SHA-256 verification against the update center's `/update-center.actual.json` works the same whether that file originates from the public `updates.jenkins.io` or the in-cluster Update Center. Plugins transferred via sneakernet tarball were verified at export time and are re-verified by the update center's store before serving.

## See also

- [Update Center](../operations/update-center.md) — the component that replaces `updates.jenkins.io`
- [Plugin packs](../config/plugin-packs.md) — OCI artifact format for distributing resolved plugin sets
- [Network policies](network-policies.md) — full NetworkPolicy reference
- [Helm install](helm-install.md) — chart values reference
- [varroactl CLI](../varroactl.md) — full `export` / `import` flag reference
- [Network Policy examples](examples/air-gapped/) — worked manifests for deny-all and allow-varroa-traffic
