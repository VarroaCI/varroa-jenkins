# Scaling and Capacity

Scale the Varroa control plane independently from Jenkins workloads. Each `Controller` remains one stateful Jenkins replica with its own persistent volume and mite sidecar.

## Default Control-Plane Capacity

| Component | Default | Scaling control |
|---|---:|---|
| Operator | 3 replicas | `operator.replicas`, `operator.maxConcurrentReconciles` |
| Gateway | 2 to 10 replicas | CPU HPA through `gateway.hpa.*` |
| BFF | 2 to 10 replicas | CPU HPA through `bff.hpa.*` |
| Dashboard | 1 replica | `frontend.replicas` |
| Dex | 1 replica | `dex.replicas` |
| NATS | 3 servers | `nats.config.cluster.*` |
| Update center | Storage-dependent | `updateCenter.replicas`, storage type, upload mode |

The BFF reads the mite state of a controllers list with at most 16 concurrent NATS requests. A member operator answers its controllers list in pages of at most 900 KB, and the BFF reads every page. The member operator reads each page from its informer cache. A page sends no list request to the Kubernetes API server. The cache can lag a recent change by about one second. The config lists (bundles, catalog sources and items, roles, bindings, version profiles, and cell pools), the brood-operation list, and the brood-schedule list page the same way.

Gateway and BFF HPAs target 70 percent CPU. They need the Kubernetes metrics API and resource requests.

One operator replica holds the leader lease. That replica serves the operator command subjects and the ledger actions, writes the cluster heartbeat, and runs the periodic reconcilers. It renews the lease every 2 seconds. It keeps the lease through a Kubernetes API outage of up to 30 seconds. The shard leases below keep their own 25-second drop rule, so a replica can drop its shards before it keeps the manager lease. A replica that loses the lease exits, and Kubernetes restarts it. After a crash, another replica takes the lease within about 50 seconds. A clean shutdown releases the lease, so a rolling update moves it within about 5 seconds. The BFF RBAC federation loop and the Tenant agent status writer use the same lease timings.

Operator replicas divide controllers through virtual shards. A replica that loses a shard stops listening to its mites. A replica gives up a shard after its work ends. It waits at most 5 seconds, or 12 seconds at shutdown. A replica drops its shards after 25 seconds without a renewal. A long rebalance pass renews the due shards between its claims, oldest first. Each lease call times out after 20 seconds. Tick writes are conditional, so a stalled replica cannot overwrite newer status. Increase `operator.maxConcurrentReconciles` only when the Kubernetes API and NATS can absorb more work. `operator.shardCount` must match across every replica. Changing it requires restarting all replicas together.

## Jenkins Capacity

Plan per controller for:

- one Jenkins pod and mite sidecar;
- one persistent `$JENKINS_HOME` volume;
- temporary plugin installation and bundle materialization work;
- agent pods created by Jenkins builds.

No CPU or memory request is applied to Jenkins unless set on the `Controller` or its `ControllerClass`. The default persistent volume is 20 GiB. Treat these as installation floors, not production sizing. Configure controller resources and persistence from observed queue depth, heap pressure, plugin load, build concurrency, and retention needs.

Jenkins controllers do not scale horizontally. Add controllers to isolate tenants or workloads, increase a controller's resources for vertical capacity, and use hibernation for idle controllers. Kubernetes agents scale separately according to the pod templates and Jenkins cloud limits in the applied bundle.

## NATS and Activity Storage

The chart defaults to a three-server NATS cluster. `jetStreamReplicas` derives from the NATS cluster size and is capped at three unless explicitly set. The chart rejects a JetStream replica count larger than the NATS server count.

Size NATS persistent storage for control-plane state and the fixed activity transport buffer. That buffer is the `varroa_activity` JetStream stream. It holds at most 72 hours or 1 GiB, whichever it reaches first. The limit is fixed in code: the buffer only hands events to the recorder and serves no history.

Durable activity history lives in Postgres, sized by `recordStore.retention.activity` (default `30d`), not by NATS. See [Build records](../operations/build-records.md#activity-history) for the retention arithmetic. If NATS loses quorum, shared-state API reads and controller convergence can stall, even when Jenkins stays available.

## Update-Center Availability

Local update-center storage uses one replica because the PVC is a single-writer store. OCI storage can use multiple replicas, but enabling uploads forces one writer. When the update center is unavailable, plugin downloads fail or provisioning waits, depending on whether online fallback is allowed.

## Capacity Checks

```bash
kubectl top pods -n varroa-system
kubectl get hpa -n varroa-system
kubectl get pvc -A
kubectl get controller -A
```

Watch reconcile latency, gateway connections, API latency, NATS storage and quorum, Jenkins heap use, and agent scheduling. See [Observability](../operations/observability.md) and [Lifecycle operations](../operations/lifecycle.md#stop-start-hibernate-and-wake).
