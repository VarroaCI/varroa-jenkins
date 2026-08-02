# Clusters

The **Clusters** page lists every registered Kubernetes cluster running the Varroa operator.
Access it from the sidebar under **Operate > Clusters** (`⛶` icon).

## Page layout

Each row shows one cluster with these columns:

| Column | Description |
|---|---|
| Cluster | Cluster name, with a **core** tag on the core cluster |
| Health | **Healthy** (green) or **Unhealthy** (red) pill |
| Heartbeat | Age of the last heartbeat (e.g. `12s ago`, `3m ago`, `2h ago`) |
| Operator | Operator version running on the cluster |
| Kubernetes | Kubernetes version of the cluster |
| Controllers | Connected count / total count (`3/5`) |
| (link) | **View controllers ›** — drill down to the cluster-filtered controllers list |

The core cluster always sorts first; remaining clusters sort alphabetically.

## Empty state

The core cluster always heartbeats, so an empty list is theoretically unreachable.
A defensive **"No clusters registered"** message is shown if the list is ever empty.

---

# Dashboard cluster health strip

When two or more clusters are registered, the **Dashboard** shows a health strip below
the page title. Each cluster gets a compact card showing:

- Health dot (green = healthy, red = unhealthy)
- Cluster name + **core** tag on the core entry
- Connected / total controller count
- Heartbeat age (e.g. `3m ago`)

Clicking a card navigates to `/controllers?cluster=<name>`.

The strip is **hidden** when:
- The clusters query is loading or has errored
- Fewer than two clusters are registered

Single-cluster installations are visually unchanged.

---

# Controllers cluster column and filtering

The **Controllers** page displays a **Cluster** column on every row showing the controller's
cluster name (mono, muted). The column appears regardless of how many clusters exist.

## Cluster filter chips

When two or more clusters are known (union of clusters seen in controller data and
from the `/clusters` endpoint), a second chip row appears below the phase chips:

- **All clusters** — clear the filter (default)
- One chip per cluster with its controller count

Clicking a chip filters the list to that cluster's controllers and sets `?cluster=<name>`
in the URL. Deep-linking `/controllers?cluster=dev-cluster` pre-selects the chip.

Chips are rendered from controller data even while the clusters query is loading,
so the page is fully functional without the `/clusters` endpoint.

## Group by cluster

The **Group by** control (top toolbar) offers three options:
- **No grouping** (default)
- **Namespace** — group by namespace (existing behavior)
- **Cluster** — group by cluster; core group first, then alphabetical

## Brood operations (cross-cluster)

Brood operations can target controllers on any cluster. Checkboxes on remote
cluster rows are fully enabled. Multi-cluster selection produces cluster-qualified
`cluster/ns/name` names that the BFF partitions per cluster.

See [Brood operations — Cross-cluster runs](brood-operations.md#cross-cluster-runs).

---

# Controller wizard cluster picker

The **Basics** step of the controller creation wizard includes a **Cluster** `<select>`
when two or more clusters are registered. The picker:

- Lists only **healthy** clusters
- Defaults to the **core** cluster (labeled with `(core)`)
- Is hidden entirely when only one cluster exists (single-cluster UX is unchanged)

If the clusters query errors, the deploy button is blocked and an error message
is shown on the Basics step: *"Cluster list unavailable — cannot create controllers"*.

---

# Remote controller behavior (D6)

Controllers running on non-core clusters have these constraints:

| Surface | Behavior |
|---|---|
| **Overview / YAML / Version** tabs | Fully functional through cluster-scoped API paths |
| **Logs card** (under Diagnostics) | Card body shows the remote empty-state string *"Logs are served only for controllers on the core cluster"* — no SSE connection is opened |
| **Embedded Jenkins** | Not offered — opens via the hive's own ingress (link-out card) |
| **External Jenkins link** | Always available (gated only on `endpoint`, not on core status) |
| **Brood operations** | Cross-cluster — remote rows are selectable; see [Brood operations — Cross-cluster runs](brood-operations.md#cross-cluster-runs) |
| **Bundle deploy targets (Composer)** | Core-only |
| **ComposedBundleDetail fan-out** | Core-only |

## Cross-references

- [Multi-cluster install / hive-mode](../install/multi-cluster.md) (C5/C6)
- [Dashboard](../) for the cluster health strip
- [Controllers](../) for cluster column, filtering, and grouping

## Namespace discovery

Each cluster's operator exposes the `operator.<cluster>.namespaces.list` subject (NATS request-reply,
queue `operator-workers`, leader-gated) for deployable-namespace discovery. The BFF's wizard fetches
`GET /api/v1/clusters/{cluster}/namespaces/deployable` to populate the namespace picker with the
target cluster's `managedNamespaces` and curated namespace list. An unreachable or pre-upgrade
hive operator shows as a degraded picker (rollout note: control-plane images roll together per D5
greenfield lockstep).
