# Adding a Member Cluster

Use full mode for the core cluster. A member cluster uses hive mode: it runs the operator and gateway, and connects to the core NATS service. It does not run a local BFF, frontend, Dex, or NATS.

## Prepare the core

Expose a TLS-protected NATS endpoint reachable from every member cluster. Set its DNS name when installing the core:

```bash
helm upgrade --install varroa charts/varroa \
  --set nats.external.enabled=true \
  --set nats.external.host=nats.example.com \
  --set nats.external.serviceType=LoadBalancer
```

Protect the endpoint with firewall rules and NATS credentials. If network policies are enabled, permit member-cluster egress to this endpoint.

## Install the member

Create in the member namespace a `varroa-nats-creds` Secret containing only `operator-password`, `gateway-password`, and `ca.crt`, copied securely from the core deployment. Do not copy private keys or BFF credentials.

```bash
helm upgrade --install varroa charts/varroa \
  -f charts/varroa/values-hive.yaml \
  --set cluster.name=<member-name> \
  --set bus.url=tls://nats.example.com:4222
```

`cluster.name` must be a DNS-1123 label and unique across the deployment. Configure the member with the same authentication issuer and settings used by controllers that connect to the core identity provider.

## Record store

A member runs no recorder and no record-store database. The core [recorder](../operations/build-records.md) serves every cluster. It reads build events and claims from the bus, and it sends each run action, such as a cell wake, to the target cluster's operator over NATS.

A member that hosts [cells](../operations/cells.md) must also write to the core record store. Its gateway writes console logs, and its cells write archived artifacts, straight to the core object store. Enable the record store in hive mode with these values:

| Value | Member setting |
|---|---|
| `recordStore.enabled` | `true` |
| `recordStore.postgres.mode` | `external`. A member renders no database. |
| `recordStore.blobs.mode` | `s3`. A cell needs the s3 tier. |
| `recordStore.blobs.s3.url` | The core bucket URL, with an `endpoint` that the member can reach |
| `recordStore.blobs.s3.existingSecret` | The Secret that holds the object store credential |
| `recordStore.blobs.s3.artifactSecret` or `artifactSTS` | The member's [artifact credentials](../operations/artifact-credentials.md) |
| `networkPolicy.recordStoreBlobEgress` | The object store address and port, when network policies are enabled |

The hive render adds the blob settings to the gateway and the operator. It renders no recorder, no CloudNativePG cluster, and no retention settings. The core keeps the retention rules for every cluster.

Copy these Secrets into the member release namespace before the install:

| Secret | Keys | Source |
|---|---|---|
| `recordStore.blobs.s3.existingSecret` | `access-key-id`, `secret-access-key`, optional `session-token` | The core Secret of the same name |
| `<artifactSecret>-<pool>`, or the STS issuer Secret | `access-key-id`, `secret-access-key` | A credential scoped to that member pool's artifact prefix, or the issuing identity |

A member in s3 blob mode needs no database credential. The gateway reads a DSN only in `postgres` blob mode, where it writes log chunks to the core database. That mode cannot host cells. For it, expose the core database and set the member DSN:

```bash
helm upgrade varroa charts/varroa -n varroa-system -f values.yaml \
  --set recordStore.postgres.memberEndpoint.enabled=true \
  --set recordStore.postgres.memberEndpoint.host=pg.example.com \
  --set recordStore.postgres.memberEndpoint.serviceType=LoadBalancer \
  --set 'recordStore.postgres.memberEndpoint.allowedCIDRs={203.0.113.0/24}'
```

The endpoint Service selects the CloudNativePG primary on port 5432. Put the core `varroa-recordstore-gateway` DSN into the member Secret named by `recordStore.postgres.external.existingSecret`, with the host replaced by the endpoint address. With network policies enabled, set `networkPolicy.recordStorePostgresEgress.cidrs` on the member. The core then requires `recordStore.postgres.memberEndpoint.allowedCIDRs`. A NodePort or load-balancer path can rewrite the source address, so list the addresses the traffic arrives from. Set `0.0.0.0/0` only when the endpoint is not reachable from outside your network.

### Pipelines tenant on a member

1. Enable the record store on the member as shown above, and confirm the gateway logs `gateway log writer enabled`.
2. Make the object store reachable from the member network. The bucket URL `endpoint` must resolve and route from the member cells and gateway.
3. With `networkPolicy.cellIsolation` enabled, add `cellIsolation.extraEgress` entries for the object store and for the update center that the member cells use.
4. Create the pool's artifact Secret (STS mode: no Secret per pool), then a `CellPool` on the member. The core `/cells` page then shows a block for the member pool, and you can scale it from the core with `varroactl scale cellpool <name> --cluster <member-name>`.
5. Create the `Team` with `spec.pipelines` on the member. The member operator places it, and the core record store holds its placement.
6. Grant access on the core with a `VarroaRoleBinding` scoped to the tenant. The core does not read the member Team's own subjects.

The core dashboard, including the `/pipelines` list, `varroactl`, and the MCP tools then read and trigger the tenant through the core. Other tenants can call it with [cross-tenant steps](../config/cross-tenant-pipelines.md). The core does not read the member Team's composed bundle, so a job's kind shows as unknown.

## Prepare controller namespaces

Each member provisions controllers using its own cluster resources. Before creating controllers, provide the required storage class, ingress setup, image pull Secrets, deployable namespaces, and `ProvisioningDefaults`. For private bundle sources, also provide the required credentials in the member cluster.

Use [Network policies](network-policies.md) to permit the gateway and core NATS traffic. For air-gapped plugin delivery, follow [Air-gapped installation](air-gapped.md) and [Update Center](../operations/update-center.md). A member that downloads plugins from the core update center sets `pluginUpdateCenterDownloadURL` to `https://<core-update-center-host>/download/plugins`.

## Verify and operate

```bash
varroactl get clusters
varroactl get controllers --cluster <member-name>
```

If the member is absent or unreachable, check its operator and gateway Pods, the NATS endpoint and credentials, TLS trust, and egress policy. Do not create controllers until the member appears healthy.

## Cluster view

The dashboard **Clusters** page lists every registered cluster with its name (a **core** tag marks the core cluster), health, heartbeat age, operator and Kubernetes versions, and connected controller count. The core cluster sorts first.

When two or more clusters are registered, the dashboard shows a cluster health strip, and the Controllers page adds a **Cluster** column plus per-cluster filter chips. The controller creation wizard offers a cluster picker that lists only healthy clusters and defaults to the core.

If a member's controller list does not arrive, the dashboard and the Controllers page name that cluster and give the reason. The cluster card in the strip shows **list unavailable**. `varroactl get controllers` prints a warning on stderr for that cluster. The Brood Operations page and `varroactl broodop list` name a member whose list did not arrive, in the same way. The Schedules page and `varroactl broodschedule get` name a member whose list did not arrive, in the same way. The MCP `list_controllers` result names that cluster in `clusters` with `ok: false`. A member that returns a controller row that the core cannot read shows **invalid controller row**, and the core shows no rows for that member.

Global search and `varroactl search` name each cluster whose results are incomplete.

Controllers on member clusters have these constraints:

| Surface | Behavior |
|---|---|
| Overview, YAML, and version tabs | Fully functional |
| Logs | Served only for controllers on the core cluster |
| Embedded Jenkins | Not offered; open Jenkins through the member's own ingress |
| Brood operations | Cross-cluster, with `cluster/namespace/name` selection |
| Bundle deploy targets | Core only |

## Drain or remove a member

Draining deletes controller resources in the target cluster. Back up Jenkins data first and confirm the StorageClass reclaim policy.

```bash
varroactl drain cluster <member-name>
varroactl drain cluster <member-name> --cancel
```

After all controllers are removed, uninstall the member release and remove only credentials that are no longer needed. A drain does not migrate Jenkins data.
