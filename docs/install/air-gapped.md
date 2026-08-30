# Install in an Air-Gapped Cluster

An offline installation needs internal copies of every chart, image, plugin, bundle, and identity endpoint used by Varroa or Jenkins.

## Mirror Artifacts

| Artifact | Include |
|---|---|
| Varroa | Chart, backend, and dashboard |
| Dependencies | Enabled NATS and Dex images |
| Jenkins | Images selected by version profiles |
| Agents | Images referenced by bundles |
| Plugins | Packs for permitted profiles |
| Configuration | Internal Git or OCI artifacts |

Render the exact values and inspect every image before transfer:

```bash
helm template varroa oci://ghcr.io/varroaci/charts/varroa \
  --version <version> -n varroa-system -f values-airgap.yaml > rendered.yaml
grep 'image:' rendered.yaml
```

## Export Plugin Packs

Run this in a connected environment for every profile that offline controllers may use:

```bash
varroactl export plugins \
  --profile jenkins-version-2-570 \
  --to tar:///transfer/plugins-2-570.tar.gz
```

The command resolves the plugin closure, downloads it, verifies checksums, and creates an OCI plugin pack. `--to` also accepts `oci://` and `dir://`. Use `--registry-config` for registry credentials. Use `--insecure` only for plain HTTP on a trusted isolated network.

## Configure Internal Images

Create the pull secret in the Varroa namespace and in every controller namespace:

```bash
kubectl create namespace varroa-system
kubectl create secret docker-registry registry-credentials \
  -n varroa-system \
  --docker-server=registry.example.com \
  --docker-username='<user>' \
  --docker-password='<password>'
```

```yaml
global:
  imagePullSecrets:
    - name: registry-credentials
operator:
  image: {repository: registry.example.com/varroa-jenkins, tag: "<version>"}
gateway:
  image: {repository: registry.example.com/varroa-jenkins, tag: "<version>"}
bff:
  image: {repository: registry.example.com/varroa-jenkins, tag: "<version>"}
frontend:
  image: {repository: registry.example.com/varroa-jenkins-frontend, tag: "<version>"}
```

Mirror and override enabled dependency images too. Set `ProvisioningDefaults.spec.imagePullSecrets` for controller pods. Each named Secret must exist in the controller namespace.

## Enable the Offline Update Center

Use local storage when one update-center replica is sufficient:

```yaml
updateCenter:
  enabled: true
  storage:
    type: local
    local:
      size: 20Gi
  pullThrough:
    enabled: false

networkPolicy:
  enabled: true
  pullThroughEgress:
    enabled: false
```

For shared registry storage, use `updateCenter.storage.type: oci` with an internal `ref` and `existingSecret`. Allow only that registry through `networkPolicy.updateCenterRegistryEgress`.

Install from an internal chart registry:

```bash
helm install varroa oci://registry.example.com/charts/varroa \
  --version <version> -n varroa-system --create-namespace \
  -f values-airgap.yaml
```

## Import Plugin Packs

```bash
export VARROACTL_UC_TOKEN=$(kubectl get secret \
  varroa-updatecenter-import-token -n varroa-system \
  -o jsonpath='{.data.token}' | base64 -d)
kubectl port-forward -n varroa-system svc/varroa-updatecenter 8080:8080
```

In another shell:

```bash
varroactl import \
  --from tar:///transfer/plugins-2-570.tar.gz \
  --to uc://localhost:8080
```

`--from` accepts `oci://`, `dir://`, and `tar://`. `uc://` is valid only for `--to` and requires `VARROACTL_UC_TOKEN`.

Require `StorageReady=True`, `CoverageComplete=True`, and `Ready=True` before provisioning offline controllers:

```bash
kubectl get updatecenter varroa-update-center -o yaml
```

`status.gaps` lists missing plugin versions. With pull-through disabled, an affected controller reports `WaitingForUpdateCenter` and remains blocked until coverage is complete. The condition message states the cause. A coverage gap points at seeding the missing plugins or setting an explicit `pluginUpdateCenterURL`/`pluginUpdateCenterDownloadURL` override in `ProvisioningDefaults`. Any other not-Ready cause points at the `varroa-update-center` resource itself.

## Enforce the Offline Boundary

The chart isolates only `varroa-system`. Apply default-deny egress and explicit allow rules in every controller namespace. Allow DNS, the Kubernetes API when needed, the mite gateway, internal registries, internal Git or OCI sources, the update center, identity services, and approved build destinations.

Test from the control-plane, Jenkins, and agent pods. Public registry and Jenkins update-service connections must fail while all internal endpoints remain reachable.

## Verify

Create a controller using an imported version profile, then check:

```bash
kubectl get controller -A
kubectl get updatecenter varroa-update-center \
  -o jsonpath='{.status.phase}{"\n"}{.status.gaps}{"\n"}'
```

The controller should reach `Connected`. Run a build that creates an agent pod to verify its image was mirrored.

| Symptom | Check |
|---|---|
| `ImagePullBackOff` | Repository, tag, architecture, and namespace pull secret. |
| Update center not ready | PVC binding or internal registry reachability. |
| Coverage incomplete | `status.gaps` and imported profile packs. |
| Import returns 401 | Import token and update-center Secret. |

See [Update Center](../operations/update-center.md) and [Plugin packs](../config/plugin-packs.md).
