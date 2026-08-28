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

Gateway and BFF HPAs target 70 percent CPU utilization and require the Kubernetes metrics API. Resource requests must remain set for CPU scaling to work.

Operator replicas divide controller ownership through virtual shards. Increase `operator.maxConcurrentReconciles` only when the Kubernetes API and NATS can absorb more concurrent work. `operator.shardCount` must match across every replica. Changing it is disruptive and requires restarting all operator replicas together.

## Jenkins Capacity

Plan per controller for:

- one Jenkins pod and mite sidecar;
- one persistent `$JENKINS_HOME` volume;
- temporary plugin installation and bundle materialization work;
- agent pods created by Jenkins builds.

No CPU or memory request is applied to Jenkins unless set on the `Controller` or its `ControllerClass`. The default persistent volume is 20 GiB. Treat these as installation floors, not production sizing. Configure controller resources and persistence from observed queue depth, heap pressure, plugin load, build concurrency, and retention needs.

Jenkins controllers do not scale horizontally. Add controllers to isolate teams or workloads, increase a controller's resources for vertical capacity, and use hibernation for idle controllers. Kubernetes agents scale separately according to the pod templates and Jenkins cloud limits in the applied bundle.

## NATS and Activity Storage

The chart defaults to a three-server NATS cluster. `jetStreamReplicas` derives from the NATS cluster size and is capped at three unless explicitly set. The chart rejects a JetStream replica count larger than the NATS server count.

Size NATS persistent storage for activity retention and control-plane state. Activity history defaults to seven days and is also bounded by `activity.maxMsgs` and `activity.maxBytes`. If NATS loses quorum, API reads that depend on shared state and controller convergence can stall even when Jenkins remains available.

## Update-Center Availability

Local update-center storage uses one replica because the PVC is a single-writer store. OCI storage can use multiple replicas, but enabling uploads forces one writer. Plugin downloads fail or provisioning waits when the configured update center is unavailable, depending on whether online fallback is allowed.

## Capacity Checks

```bash
kubectl top pods -n varroa-system
kubectl get hpa -n varroa-system
kubectl get pvc -A
kubectl get controller -A
```

Watch reconcile latency, gateway connections, API latency, NATS storage and quorum, Jenkins heap use, and agent scheduling. See [Observability](../operations/observability.md) and [Lifecycle operations](../operations/lifecycle.md#stop-start-hibernate-and-wake).
