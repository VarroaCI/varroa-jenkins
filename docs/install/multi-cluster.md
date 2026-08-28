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

## Prepare controller namespaces

Each member provisions controllers using its own cluster resources. Before creating controllers, provide the required storage class, ingress setup, image pull Secrets, deployable namespaces, and `ProvisioningDefaults`. For private bundle sources, also provide the required credentials in the member cluster.

Use [Network policies](network-policies.md) to permit the gateway and core NATS traffic. For air-gapped plugin delivery, follow [Air-gapped installation](air-gapped.md) and [Update Center](../operations/update-center.md).

## Verify and operate

```bash
varroactl get clusters
varroactl get controllers --cluster <member-name>
```

If the member is absent or unreachable, check its operator and gateway Pods, the NATS endpoint and credentials, TLS trust, and egress policy. Do not create controllers until the member appears healthy.

## Cluster view

The dashboard **Clusters** page lists every registered cluster with its name (a **core** tag marks the core cluster), health, heartbeat age, operator and Kubernetes versions, and connected controller count. The core cluster sorts first.

When two or more clusters are registered, the dashboard shows a cluster health strip, and the Controllers page adds a **Cluster** column plus per-cluster filter chips. The controller creation wizard offers a cluster picker that lists only healthy clusters and defaults to the core.

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
