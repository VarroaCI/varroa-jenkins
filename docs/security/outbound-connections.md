# Outbound connections

<!-- Generated from hack/checkoutbound/inventory.yaml by make outbound-doc. Do not edit this page. -->

This page lists each destination that a Varroa component dials.
Use it to build an egress allowlist.
Use it also to show an accreditor where traffic goes.

Each row names one destination. The Control column names the chart value or the resource field that changes the destination. The last column states whether a deny-all cluster can turn the connection off.

A build check fails when a source adds a host that this page does not list.

## Operator

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| updates.jenkins.io | Find newer LTS patches and resolve candidate plugin sets for the upgrade tracker. | HTTPS 443 | jenkins.upstreamURL | Yes. Set jenkins.upstreamURL to "". |
| updates.jenkins.io (update-center metadata) | Read update-center metadata when pull-through uses the default upstream. | HTTPS 443 | updateCenter.pullThrough.upstreamURL | Yes. Pull-through is off by default. Keep updateCenter.pullThrough.enabled false. |
| Git remotes of catalog sources and bundle inputs | Clone catalog sources and git bundle inputs. | HTTPS 443 or SSH 22 | CatalogSource.spec.repoURL and ComposedBundle gitSource inputs | Yes. Do not declare the source or the input. |
| OCI registries of catalog sources and bundle inputs | Pull catalog and bundle artifacts. | HTTPS 443 | CatalogSource.spec.ociRef and ComposedBundle ociSource inputs | Yes. Do not declare the source or the input. |
| api.github.com | Read a proposed repository and open a pull request on GitHub. | HTTPS 443 | the repo field of POST /pipelines/{team}/propose and of the open_pull_request tool, and a tenant credential's varroa.dev/allowed-hosts | Yes. Create no tenant credential that lists github.com. |
| Forge API of a proposed repository | Read a proposed repository and open a pull request on a Gitea-API forge. | HTTPS 443 | the repo field of POST /pipelines/{team}/propose and of the open_pull_request tool, and a tenant credential's varroa.dev/allowed-hosts | Yes. Create no tenant credential whose varroa.dev/allowed-hosts lists a non-GitHub forge host. |

## Gateway

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| None outside the cluster | Serves mite gRPC and API key verification. It dials NATS inside the cluster only. | gRPC 9090 and 9092, NATS 4222 | Not applicable | Not applicable. |

## BFF

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| OIDC issuer (auth.oidc.issuer) | Discover the sign-in provider keys and endpoints. | HTTPS 443 | auth.oidc.issuer | Yes, with a local issuer (auth.mode local or ldap). Set the mode in the chart. |
| Record store (external Postgres) | Read build history from the record store. | PostgreSQL 5432 | recordStore.postgres.mode and host | Yes. Leave recordStore.enabled false. |
| api.anthropic.com | Send assistant requests to the Anthropic Messages API when a provider of kind anthropic leaves baseURL empty. | HTTPS 443 | assistant.providers[].baseURL (the adapter default when it is empty) | Yes. Keep assistant.enabled false. |
| Configured assistant provider base URLs | Send assistant requests to the configured model provider. | HTTPS 443, or the port in the base URL | assistant.providers[].baseURL | Yes. Keep assistant.enabled false. |

## Update center

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| updates.jenkins.io | Pull plugin metadata and HPI files from the upstream update center on demand. | HTTPS 443 | updateCenter.pullThrough.upstreamURL and downloadURL | Yes. Keep updateCenter.pullThrough.enabled false. The default is false. |
| repo.jenkins-ci.org | Recover checksums for plugin versions that the update-center metadata no longer lists. | HTTPS 443 | updateCenter.pullThrough.archiveURL | Yes. Set updateCenter.pullThrough.archiveURL to "". Aged pins then report as gaps. The egress rule defaults to 0.0.0.0/0 on 443. |
| OCI registry (storage type oci) | Store plugin packs in an external OCI registry. | HTTPS 443 | updateCenter.storage | Yes. Use local storage. With oci storage, networkPolicy.updateCenterRegistryEgress.cidrs must name the registry. |

## Recorder

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| Record store (external Postgres) | Write build history and the build-number ledger. | PostgreSQL 5432 | recordStore.postgres.mode and host | No, while the record store runs. |
| S3-compatible blob store (recordStore.blobs.s3.url) | Store build logs and artifacts in object storage. | HTTPS 443 | recordStore.blobs.mode and s3.url | Yes. Set recordStore.blobs.mode to postgres. In s3 mode, networkPolicy.recordStoreBlobEgress.cidrs must name the store. |
| STS endpoint (recordStore.blobs.s3.artifactSTS.endpoint) | Issue a scoped artifact credential for each cell. | HTTPS 443 | recordStore.blobs.s3.artifactSTS.endpoint | Yes. Set artifactSTS.enabled to false. |
| Completion webhook URL of a Team | Post a signed message when a tenant run completes. | HTTPS 443 | Team spec.pipelines.notifications.webhook and networkPolicy.recorderWebhookEgress | Yes. Leave the webhook unset. The webhook egress rule is off by default. |

## Mite

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| Gateway service inside the cluster | Send heartbeats and receive commands over gRPC with mTLS. | gRPC 9090, mTLS | Chart service name | Not applicable. The service runs inside the cluster. |

## Jenkins controller

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| updates.jenkins.io (Jenkins update site) | Jenkins core reads update-site and tool-installer metadata at boot, once a day, and after a version change. | HTTPS 443 | ProvisioningDefaults pluginUpdateCenterURL, the in-cluster update center, or jenkins.upstreamURL | Yes. Enable the in-cluster update center, or set jenkins.upstreamURL to "". Varroa then writes an empty update-site list. |
| usage.jenkins.io | The viewer browser sends usage statistics when the Jenkins usage setting is on. | HTTPS 443 | None needed | Yes. Off on every controller: the JVM starts with -Dhudson.model.UsageStatistics.disabled=true. |
| uplink.jenkins.io | Jenkins core sends server telemetry when the usage setting is on. | HTTPS 443 | None needed | Yes. Off on every controller: the JVM starts with -Dhudson.model.UsageStatistics.disabled=true. |
| archives.jenkins.io | plugins-init falls back to the archive for each failed plugin download. | HTTPS 443 | None | No. The plugins-init script always runs. |
| Plugin update center (plugins-init) | Download plugins from the update center named in the JENKINS_UC variables. | HTTPS 443 | ProvisioningDefaults and the in-cluster update center | Yes, when an in-cluster update center serves every plugin. Otherwise No. |
| OIDC issuer (bundle variable) | The OIDC plugin discovers the sign-in provider. | HTTPS 443 | auth.oidc.issuer | Yes, with a local issuer (auth.mode local or ldap). |
| Gateway API key verification | Check an API key against the gateway inside the cluster. | HTTPS 9092 | Operator endpoint setting (VARROA_APIKEY_VERIFY_URL overrides it) | Not applicable. The gateway runs inside the cluster. |
| CellPool library remotes | Clone trusted shared libraries into each cell of a pool. | HTTPS 443 or SSH 22 | CellPool spec.libraries | Yes. Remove the libraries from the pool. |
| SMTP server (CellPool mail) | Send mail from pipeline steps. | SMTP | CellPool spec.mail | Yes. Leave spec.mail unset. |
| Gitea servers (CellPool giteaServers) | Read repositories from Gitea hosts that a pool names. | HTTPS 443 | CellPool spec.giteaServers | Yes. Leave spec.giteaServers unset. |

## Jenkins agents

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| External runner machines (SSH) | The cell dials out over SSH to each external runner. | SSH 22 | Runner spec | Yes. Delete the Runner object. |

## Dex

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| Dex connector targets | Check user credentials and groups with upstream identity providers. | HTTPS 443 or LDAP 636 | dex.config.connectors | Yes. Remove the connectors and use a local connector. |

## NATS

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| Hive NATS (bus.url) | Link a member cluster to the core bus. | NATS 4222, TLS | bus.url | Yes. Set bus.url to "". |

## varroactl

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| updates.jenkins.io (plugin download) | Download plugin archives for an export. | HTTPS 443 | export plugins --download-url-base | Yes. Pass a local download base or use prebuilt packs. |
| repo.jenkins-ci.org (bootstrapdeps) | Maintainer plugin resolver. It is not in any image. | HTTPS 443 | Run by hand with --resolve only | Yes. Do not run the maintainer command. |
| Git remote of an export bundle | Clone the bundle repository for export bundle. | HTTPS 443 or SSH 22 | The repository argument of export bundle | Yes. Do not run the command. |
| Registry of an OCI source or target | Read or write images, charts, and plugin packs in an OCI registry. | HTTPS 443 | The oci reference given on the command line | Yes. Do not run the command. |
| Git remote of an export catalog | Clone the catalog source for export catalog. | HTTPS 443 or SSH 22 | The CatalogSource that export catalog reads | Yes. Do not run the command. |

## Cluster-wide

| Destination | Purpose | Protocol | Control | Can a deny-all cluster disable it? |
| --- | --- | --- | --- | --- |
| ghcr.io (Varroa images) | Pull the operator, gateway, BFF, update center, recorder, mite, and frontend images. | HTTPS 443 | Each component's image repository and tag. The mite image is set by VARROA_MITE_IMAGE. | No. Mirror the images and set the repositories. |
| ghcr.io (Dex image) | Pull the Dex image. | HTTPS 443 | dex.image.repository and tag | No. Mirror the image and set the repository. |
| docker.io (jenkins/jenkins) | Pull the Jenkins controller image for each controller. | HTTPS 443 | jenkins.image.repository | No. Mirror the image and set the value. |
| docker.io (jenkins/inbound-agent) | Pull the Jenkins agent image for starter and cell pods. | HTTPS 443 | jenkins.agentImage | No. Mirror the image and set the value. |
| docker.io (nats) | Pull the NATS server image. | HTTPS 443 | nats.container.image.repository and tag | No. Mirror the image and set the repository. |
| docker.io (NATS subchart images) | Pull the NATS box and config reloader images. | HTTPS 443 | NATS subchart values | No. Mirror the images and set the subchart values. |
| ghcr.io (CloudNativePG operator and database images) | Pull the database images in cnpg mode. | HTTPS 443 | recordStore.postgres.mode | Yes. Set recordStore.postgres.mode to external. |
| OTLP collector (telemetry.endpoint) | Export traces, metrics, and logs. | OTLP gRPC, no TLS | telemetry.endpoint | Yes. Leave telemetry.endpoint "". No export runs. |

## Destinations this page cannot list

The check reads the Go, chart, bundle, plugin, frontend, and example sources. It does not assign a host to a component. It cannot list these destinations:

- Images and hosts in pipeline scripts. A tenant defines them at run time.
- Traffic from tenant builds. Each build sends the traffic that its own steps need.
- The database image that CloudNativePG chooses when the chart sets no image.

To mirror the images and plugins for a cluster with no internet access, follow [Air-gapped installs](../install/air-gapped.md).
