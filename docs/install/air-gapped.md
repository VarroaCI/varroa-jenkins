# Install in an Air-Gapped Cluster

An offline installation needs internal copies of every chart, image, plugin, bundle, and identity endpoint used by Varroa or Jenkins.

## Transfer One Bundle

Export one bundle on a connected machine. Import it on the disconnected side. The bundle holds the chart, the images that the chart uses, the plugin packs of the profiles that you choose, and a manifest. The manifest records a digest for every file.

The export needs three things on the connected side:

- A connected Varroa of the same version. The export reads the version profiles from its API.
- The `helm` binary. The export renders the chart with it. Use `--helm` to name another path.
- Your values files. Use the files that you deploy with. The export then reads the same images as the install.

The values files choose the image set. They do not choose the configuration that the bundle carries. The chart does not render without some required values, such as `auth.oidc.clientSecret`, `auth.oidc.clientId`, and `auth.dashboardUrl`. In `local` or `ldap` auth mode, `auth.bffUrl` is required too. On the connected machine, set a placeholder for each required value in the export values file. A placeholder is correct for the export. Install the disconnected cluster with the real values. The manifest records only the file name and the SHA-256 digest of each values file. It never records the content of a values file.

```bash
varroactl export release \
  --version 3.0.0 \
  --values values-prod.yaml \
  --profile jenkins-version-2-570 \
  --to tar:///transfer/varroa-3.0.0.tar.gz
```

The command prints a manifest digest. Record it. The import needs it.

Send the manifest digest to the disconnected side over a channel that is separate from the archive. The digest is the trust anchor. An archive that carries its own digest proves nothing, because anyone who alters the archive can alter the digest too.

Carry the archive across the boundary. Then run the import on the internal side:

```bash
varroactl import release \
  --from tar:///transfer/varroa-3.0.0.tar.gz \
  --to oci://registry.example.com/varroa \
  --manifest-digest sha256:<manifest digest>
```

The import checks the manifest digest, every blob, and every artifact tag before it pushes anything. A failed check stops the import, and nothing is pushed.

A registry or network failure during the push is different. The artifacts that were already pushed stay in the registry, and the import reports how many completed. Run the same import again. A rerun of the same bundle is safe: it pushes the same digests under the same names. The import then prints a values file. Save it, and install from the internal registry with it:

```bash
helm upgrade --install varroa oci://registry.example.com/varroa/charts/varroa \
  --version 3.0.0 -f values-prod.yaml -f values-airgap.yaml
```

Add `--uc uc://localhost:8080` to the import to upload the plugin packs to the update center. The [Import Plugin Packs](#import-plugin-packs) section shows how to reach the update center.

Use `--registry-config` for registry credentials. Use `--insecure` only for plain HTTP on a trusted, isolated network. The commands read registry credentials from the Docker config. `--registry-config <file>` names a plain config file instead. A credential helper that fails makes a registry fetch fail with `exit status 1`. A `dir://` bundle works on removable media. An `oci://` destination pushes the bundle to a staging registry, but the import reads only `tar://` and `dir://` sources.

### Rules the export enforces

- Every image needs an exact tag or a digest. The export refuses the tag `latest`. Release the chart with pinned image tags.
- Every image must come from a chart value, a profile, a `--bundle-dir`, or an `--image` flag. The export refuses any other image.

### What the export cannot see

The export does not read these image sources:

- Images in pipeline scripts and in pod template steps.
- Images in Team and CellPool pod templates that users create through the API.
- Images that plugins pull at run time.

Name a bundle directory with `--bundle-dir` to include the JCasC pod templates that name images. Use `--image` for any other image. The manifest lists these sources under `unseen`. Mirror them by hand.

### What the bundle does not carry

The bundle carries the Varroa chart, its images, and the plugin packs. It does not carry the following. Mirror each one by hand, and point the cluster at the internal copy:

- Git and OCI sources for JCasC bundles, such as the `repoURL`, `ociRef`, `gitSource`, and `ociSource` fields.
- Catalog sources and the catalog items that they serve. Use `varroactl export catalog` to copy a catalog source to a transfer archive.
- Identity endpoints: Dex connectors and the OIDC issuer.
- The record store: an external Postgres server and the S3 blob endpoint.
- Telemetry endpoints, completion webhook targets, and the mail and Gitea servers that a cell pool uses.

Find every other destination in the [outbound connections](../security/outbound-connections.md) page before transfer.

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

- **The operator.** The upgrade tracker does not start. The operator makes no request to the upstream host to find newer LTS patches or to resolve candidate plugin sets. The upgrade tracker creates no candidates. Settings ▸ Versions shows a note that upstream metadata is off, and `GET /api/v1/version-candidates` returns `upstreamMetadata: "off"`. A plugin pack import into an update center with `oci` storage resolves an existing candidate that reports `Resolved=False` with reason `UpstreamMetadataDisabled`. The import does not add a candidate.
- **Every Jenkins controller and cell.** Each JVM starts with `-Dhudson.model.UpdateCenter.never=true`, which stops the daily update-site poll. Each JVM also starts with `-Dhudson.model.DownloadService.never=true`, which stops no request in current Jenkins. The runtime update site is set separately, as described below.
- **Usage statistics, always.** Each Jenkins JVM starts with `-Dhudson.model.UsageStatistics.disabled=true`. This flag applies whether or not the upstream host is set. It stops the calls to `usage.jenkins.io` and to the telemetry uplink.

A change to `jenkins.upstreamURL` rolls every Connected controller and cell once. The new pod gets the matching JVM flags. A cell drains its builds before the roll. A hibernated controller gets the flags when it wakes.

The empty value does not change the plugins-init step. That init container still installs plugins from the update center in `ProvisioningDefaults`, or from the in-cluster update center. Set that source as described in [Enable the Offline Update Center](#enable-the-offline-update-center).

The in-cluster update center has its own default host for pull-through. Turn off pull-through as described in [Enable the Offline Update Center](#enable-the-offline-update-center), or set its upstream URLs to your mirror. This value does not change that default.

Varroa sets the runtime update site through JCasC. The site is `ProvisioningDefaults.spec.pluginUpdateCenterURL` when set. Otherwise it is the in-cluster update center, and then `jenkins.upstreamURL`. With none of these, the site list is empty. A `pluginUpdateCenterURL` must end in `update-center.json` or `update-center.actual.json`; Varroa rewrites the second form to the first for Jenkins. Other names do not load tool-installer metadata. The in-cluster update center does not sign its metadata, so Jenkins does not load it. The controller's plugin manager shows no available plugins. Plugins come only from the bundle.

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

For shared registry storage, use `updateCenter.storage.type: oci` with an internal `ref` and `existingSecret`. With oci storage, set `networkPolicy.updateCenterRegistryEgress.cidrs` to that registry's CIDR. The render fails without it.

Install from an internal chart registry:

```bash
helm install varroa oci://registry.example.com/charts/varroa \
  --version <version> -n varroa-system --create-namespace \
  -f values-airgap.yaml
```

## Import Plugin Packs

This section transfers plugin packs only. The release bundle above uploads the packs for you through `import release --uc`.

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

The chart isolates only `varroa-system`. Pin `networkPolicy.apiServerEgress`, `networkPolicy.gitEgress`, and `networkPolicy.dexConnectorEgress` to internal addresses too. Their defaults allow every address on 443. Use `examples/air-gapped/values-airgap.yaml` as the model, and the [egress table](network-policies.md#restrict-external-egress).

Apply default-deny egress and explicit allow rules in every controller namespace. Allow DNS, the Kubernetes API when needed, the mite gateway, internal registries, internal Git or OCI sources, the update center, identity services, and approved build destinations.

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
