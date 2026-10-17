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

Find every destination in the [outbound connections](../security/outbound-connections.md) page before transfer. The page lists each image registry, each external host, and the chart value that controls it.

## Export Plugin Packs

Run this in a connected environment for every profile that offline controllers may use:

```bash
varroactl export plugins \
  --profile jenkins-version-2-570 \
  --to tar:///transfer/plugins-2-570.tar.gz
```

The command resolves the plugin closure, downloads it, verifies checksums, and creates an OCI plugin pack. The pack records concrete versions; a `latest` pin resolves at export time. `--to` also accepts `oci://` and `dir://`. Use `--registry-config` for registry credentials. Use `--insecure` only for plain HTTP on a trusted isolated network.

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
  image:
    pullSecretNames: [registry-credentials]  # every pod, NATS too
operator:
  image: {repository: registry.example.com/varroa-jenkins, tag: "<version>"}
gateway:
  image: {repository: registry.example.com/varroa-jenkins, tag: "<version>"}
bff:
  image: {repository: registry.example.com/varroa-jenkins, tag: "<version>"}
frontend:
  image: {repository: registry.example.com/varroa-jenkins-frontend, tag: "<version>"}
jenkins:
  image: {repository: registry.example.com/jenkins/jenkins}
  agentImage: registry.example.com/jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21
```

Mirror and override enabled dependency images too. NATS reads `nats.container.image`. Set `ProvisioningDefaults.spec.imagePullSecrets` for controller pods. Each named Secret must exist in the controller namespace.

`jenkins.image.repository` sets the Jenkins controller image. The operator adds the core version as the tag. `jenkins.agentImage` sets the jnlp agent image for the starter bundle and for cell pod templates. Mirror the exact agent tag, as shown in the starter bundle. A bundle that names its own agent image bypasses this value.

## Turn Off Upstream Reads

Set `jenkins.upstreamURL` to an empty string. The default value reads `https://updates.jenkins.io`.

```yaml
jenkins:
  upstreamURL: ""
```

The empty value stops these reads:

- **The operator.** The upgrade tracker does not start. The operator makes no request to the upstream host to find newer LTS patches or to resolve candidate plugin sets. A candidate that needs upstream metadata reports `Resolved=False` with reason `UpstreamMetadataDisabled`. Import the candidate's plugin pack into an update center with `oci` storage to resolve it.
- **Every Jenkins controller and cell.** Each JVM starts with `-Dhudson.model.UpdateCenter.never=true` and `-Dhudson.model.DownloadService.never=true`. The JVM does not poll its update site or download plugin metadata on a timer.
- **Usage statistics, always.** Each Jenkins JVM starts with `-Dhudson.model.UsageStatistics.disabled=true`. This flag applies whether or not the upstream host is set. It stops the calls to `usage.jenkins.io` and to the telemetry uplink.

A controller created with this setting gets the flags. A running controller keeps its JVM options until it is provisioned again, for example after a version change. Varroa does not update these options on a running controller.

The empty value does not change the plugins-init step. That init container still installs plugins from the update center in `ProvisioningDefaults`, or from the in-cluster update center. Set that source as described in [Enable the Offline Update Center](#enable-the-offline-update-center).

The in-cluster update center has its own default host for pull-through. Turn off pull-through as described in [Enable the Offline Update Center](#enable-the-offline-update-center), or set its upstream URLs to your mirror. This value does not change that default.

The JVM flags do not set a runtime update site. A controller that has no update-site data file can still download it from the default host on first start. Varroa does not yet point the runtime update site at the in-cluster update center. Until it does, check the egress policy for this one request.

Check the operator log after the change. It contains `upgrade tracker off: jenkins upstream URL is empty` once.

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

Require `StorageReady=True`, `PluginsServable=True`, and `Ready=True` before provisioning offline controllers. Seed every plugin in `status.gaps` before the cutover:

```bash
kubectl get updatecenter varroa-update-center -o yaml
```

`status.gaps` lists missing plugin versions, and `status.unservable` lists the ones the update center cannot serve, each with a reason. A `latest` pin needs one stored version of the plugin, because pull-through is off. With pull-through disabled, an affected controller reports `WaitingForUpdateCenter` and remains blocked until `PluginsServable` is `True`. The condition message states the cause. A coverage gap points at seeding the missing plugins or setting an explicit `pluginUpdateCenterURL`/`pluginUpdateCenterDownloadURL` override in `ProvisioningDefaults`. Any other not-Ready cause points at the `varroa-update-center` resource itself. An explicit download URL must end in `/download/plugins`. Otherwise every plugin falls back to the public archive, which an offline pod cannot reach. See [Update Center](../operations/update-center.md#set-an-explicit-download-url).

## Fill the Store Before an Offline Cutover

Prefetch fetches every declared plugin that pull-through can serve. Use it to fill the store before you disconnect the cluster.

1. Set `updateCenter.pullThrough.enabled=true` and `updateCenter.pullThrough.prefetch=true`.
2. Wait until `PluginsPrefetched` and `CoverageComplete` are True. Run `kubectl get updatecenter varroa-update-center -o yaml` and read both conditions.
3. Set `updateCenter.pullThrough.enabled=false` and `updateCenter.pullThrough.prefetch=false`. Disable `networkPolicy.pullThroughEgress` too.
4. Check that `PluginsServable` is True.

The hosts named in `updateCenter.pullThrough.upstreamURL`, `downloadURL`, and `archiveURL` must stay reachable until step 2 completes. A plugin whose host is blocked stays a gap.

## Enforce the Offline Boundary

The chart isolates only `varroa-system`. Apply default-deny egress and explicit allow rules in every controller namespace. Allow DNS, the Kubernetes API when needed, the mite gateway, internal registries, internal Git or OCI sources, the update center, identity services, and approved build destinations.

Test from the control-plane, Jenkins, and agent pods. Public registry and Jenkins update-service connections must fail while all internal endpoints remain reachable.

## Verify

Create a controller using an imported version profile, then check:

```bash
kubectl get controller -A
kubectl get updatecenter varroa-update-center \
  -o jsonpath='{.status.phase}{"\n"}{.status.unservableCount}{"\n"}{.status.unservable}{"\n"}'
```

The controller should reach `Connected`. Run a build that creates an agent pod to verify its image was mirrored.

| Symptom | Check |
|---|---|
| `ImagePullBackOff` | Repository, tag, architecture, and namespace pull secret. |
| Update center not ready | PVC binding or internal registry reachability. |
| Plugins cannot be served | `status.unservable` (reason per plugin) and imported profile packs. |
| Import returns 401 | Import token and update-center Secret. |
| Import returns 400 and names a plugin at latest | Export the pack again. A pack must hold concrete versions. |

See [Update Center](../operations/update-center.md) and [Plugin packs](../config/plugin-packs.md).
