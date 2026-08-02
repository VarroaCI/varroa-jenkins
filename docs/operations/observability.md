# Observability

<!-- sources: cmd/operator/main.go (:9091 metrics), cmd/gateway/main.go (grpcPort+1 metrics), cmd/bff/main.go (/metrics, /healthz), internal/api/router.go (/activity, /activity/stream), charts/varroa/values.yaml (activity.*, prometheus.*, grafana.*, telemetry.*) -->

Watching the brood: metrics, health endpoints, and the activity feed. The chart optionally bundles Prometheus and Grafana pre-wired to all of it.

Per-controller telemetry is available from the controller workspace's Observability tab. Operational events are under Diagnostics > Activity, while Diagnostics > Logs opens a live stream only while that nested view is selected. Leaving Logs closes the stream; returning reconnects it, and the browser retains at most 500 lines. Diagnostics also keeps connection details and conditions. The former read-only CRD/YAML view is removed — raw structured editing now lives in **Configuration → Spec Editor** (see [Pod customization](../config/pod-customization.md#the-spec-editor-dashboard-ui)).

## Reference: endpoints per component

| Component | Metrics | Health |
|---|---|---|
| operator | `:9091/metrics` | `:9091/healthz` |
| gateway | `:9091/metrics` (gRPC port + 1) | `:9091/healthz` |
| bff | `:8080/metrics` | `:8080/healthz` |

All metrics endpoints are Prometheus format. The bundled Prometheus (`prometheus.enabled`) scrapes them out of the box; Grafana (`grafana.enabled`) ships with brood dashboards. Grafana's admin password is auto-generated into `<release>-grafana-admin` and printed in `helm install`/`upgrade` NOTES output. Retrieve it with `kubectl -n <namespace> get secret <release>-grafana-admin -o jsonpath='{.data.admin-password}' | base64 -d; echo` to log into the Grafana UI. OTLP trace/metric export to your own collector is available via `telemetry.endpoint` (renders the [network-policy egress](../install/network-policies.md) automatically).

## How to scrape with your own Prometheus

Static scrape config against the component Services:

```yaml
scrape_configs:
  - job_name: varroa
    kubernetes_sd_configs: [{ role: pod, namespaces: { names: [varroa] } }]
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app_kubernetes_io_component]
        regex: operator|gateway|bff
        action: keep
```

Point the operator/gateway jobs at `:9091` and bff at `:8080`.

**Verify:** `up{job="varroa"}` is `1` for every replica; you can query mite-connection and reconcile metrics.

## Concepts: the activity feed

Every meaningful brood event — phase transitions, applies, approvals, drains, errors — is published as an activity event and served two ways:

- **Backfill** — `GET /api/v1/activity` reads history from the JetStream stream `varroa_activity` (per-controller subjects are `activity.<cluster>.<namespace>.<controller>`, plus a global feed).
- **Live** — `GET /api/v1/activity/stream` is SSE; the dashboard's activity page runs on it.

Retention is the chart dial ([Scaling](../architecture/scaling.md#activity-stream-sizing)):

```yaml
activity:
  retention: "7d"      # off | 7d | 30d | 90d
  maxMsgs: 100000
  maxBytes: 1073741824
```

`off` keeps only an in-memory ring per BFF replica — no history across restarts, and different replicas may show different lists. Use a persistent setting in production.

**Durability:** the `varroa_activity` stream is replicated to `jetStreamReplicas` (default: the NATS cluster size, clamped 1..3). On a 3-node NATS cluster the history survives a single NATS pod (and PVC) loss; at R1 it is pinned to one pod. See [NATS JetStream replication](../install/helm-install.md#nats-jetstream-replication).

**History horizon after the multicluster upgrade:** Per-controller activity history begins at the upgrade to the multicluster-capable release — events recorded before the upgrade use the old subject naming and cannot be attributed to a cluster, so per-controller views never show them. Pre-upgrade events can transiently appear in the unfiltered global feed without a cluster attribution until they fall outside the newest-events window or age out. The leftover messages clean themselves up via the `activity.retention` dial (default `7d`, at most `90d`) and the size caps — no action is required and there is deliberately no migration.

## How to follow one controller's activity

```bash
curl -sf -H "Authorization: Bearer $VARROA_API_KEY" \
  "https://app.example.com/api/v1/activity?namespace=teams-platform&controller=demo&limit=50" | jq '.[].message'

# live tail:
curl -sfN -H "Authorization: Bearer $VARROA_API_KEY" \
  "https://app.example.com/api/v1/activity/stream?namespace=teams-platform&controller=demo"
```

**Verify:** trigger a reconcile ([Lifecycle](lifecycle.md)) and watch the corresponding events arrive on the stream within seconds.

Activity access is authorization-filtered: users see events only for controllers their [RBAC](../security/varroa-rbac.md) lets them read.

## Concepts: what to alert on

Practical starting set:

- `healthz` failures / pod restarts on any control-plane component.
- Controllers not `Connected`: alert on phase != Connected for > 10 minutes (`kubectl`-based or via metrics).
- Mite disconnect events in the activity feed (a burst usually means gateway or network trouble).
- Rollouts blocked beyond your bake-time budget (`status.rollout.blockedSince` aging — see [Rollout waves](rollout-waves.md)).
- `MiteImageStale` controller condition (True = `ReasonMiteImageStale`) and the `varroa_controller_mite_image_stale` gauge (labeled `namespace`, `controller`): the controller's actually-running mite sidecar image differs from the operator-desired image. Detection only — the operator never auto-rolls or restarts on this condition. On controller deletion the operator records a final `0` for that label pair; with the OpenTelemetry synchronous gauge the series resets to `0` and persists (at `0`) until the operator process restarts, rather than being physically removed from the exposition — so alert on `== 1`, not on series presence.
- NATS JetStream storage nearing `maxBytes` (activity discards oldest first — silent history loss, not an outage).

## Troubleshooting

- Empty activity page → retention `off` with a recently restarted BFF, or the user's RBAC filters everything.
- Controller activity page looks empty right after the multicluster upgrade → expected; pre-upgrade history is not cluster-attributable and history rebuilds from the upgrade forward (see "History horizon after the multicluster upgrade" above).
- Metrics endpoint 401 → the chart sets `METRICS_TOKEN` (via `telemetry.metricsToken`) by default; scrape with `Authorization: Bearer <telemetry.metricsToken>`, or set `telemetry.metricsToken: ""` to disable metrics auth.
- SSE stream drops behind a proxy → the ingress must allow long-lived responses (disable proxy buffering for `/api/v1/activity/stream`).

## Related pages

- [Scaling](../architecture/scaling.md) — sizing the activity stream and when to scale on what you see
- [Troubleshooting](troubleshooting.md) — turning signals into diagnoses
- [Lifecycle](lifecycle.md) — the actions activity events narrate
# Activity investigation

The Activity page combines retained history with the live SSE stream. Filters for time range,
controller, namespace, source, severity, actor, and exact event type are stored in the URL so an
investigation can be shared. Use **Load more history** to follow the opaque cursor; a `410` means
the cursor aged out of retention and the investigation must be restarted.

Activity retention is reported by the API as 7, 30, or 90 days. When retention is `off`, the page
shows only the current BFF replica's in-memory buffer and hides historical range controls. Actor
choices are derived only from events the current user is authorized to read.

## Fleet plugin queries

Two `GET` routes answer the question *"what plugins are installed across my fleet?"*:

| Route | Answers |
|---|---|
| `GET /api/v1/fleet/plugins` | One item per plugin name, with controller count, version spread, and per-class breakdown. |
| `GET /api/v1/fleet/plugins/{name}` | Every controller running that plugin — cluster, namespace, version, provenance class, and quality qualifiers. |

Both accept the same query parameters: `cluster`, `namespace`, and `affected`. The rollup also
accepts `q` for a case-insensitive substring filter on plugin name. Every parameter narrows results
only and never widens them beyond the caller's readable set.

### The `affected` version-range expression

```
expr    := clause ("," clause)*      (comma = logical AND)
clause  := op operand
op      := "<=" | ">=" | "!=" | "<" | ">" | "="
operand := any non-empty version string
```

Operators are matched longest-prefix-first, so `<=2.0` is `<=` with operand `2.0`, never `<` with
operand `=2.0`. A clause matches an installed version `v` when the installed version compares to the
operand under the clause's operator using the same `ComparableVersion` ordering Jenkins resolves
`Plugin-Dependencies` entries against. There is no `unknown` match verdict — every version string is
orderable, so every clause is either true or false.

Advisory phrasing maps directly onto the grammar:

| Advisory says | Use |
|---|---|
| "X and earlier" | `<=X` |
| "fixed in Y" | `<Y` |

Only an absent `affected` parameter means "no filter." A parameter that is present but blank, a
clause with no operator, or a clause with an empty operand each respond `400` and cost no cluster
read.

#### Worked example: SECURITY-4242, `git-client` before 4.1.0

1. Open the Plugins page and enter `git-client` in the plugin name filter.
2. Enter `<=4.0.0` in the version-range filter — the advisory says "fixed in 4.1.0," so every
   installed version below 4.1.0 is affected.
3. The rollup shows `git-client` with a controller count reflecting only controllers running a
   version `<=4.0.0`. Click the row to open the drilldown — every matching controller is listed with
   its exact version, provenance class, and quality qualifiers.
4. Copy the URL from the browser. The query (`/plugins?q=git-client&affected=<=4.0.0&plugin=git-client`)
   is a shareable link that reproduces exactly these filters. Paste it into the incident channel.

### Coverage

Every response carries a `coverage` block beside `items` that discloses how much of the fleet was
observable:

| Field | Meaning |
|---|---|
| `complete` | `false` whenever any cluster could not be reached. |
| `controllersTotal` | Caller-readable controllers in clusters that answered. |
| `controllersReporting` | Of those, how many have an observed plugin inventory. |
| `controllersStale` | Of those reporting, how many the controller marks `stale`. |
| `controllersDegraded` | Of those reporting, how many the controller marks `degraded` (classification ran without the `jenkins-supplied` class). |
| `controllersTruncated` | Of those reporting, how many the controller marks `truncated` (dependency closure indeterminate). |
| `controllersDetailStale` | Of those reporting, how many failed the classified-envelope cross-check — their provenance labels predate the summary beside them. |
| `controllersMissing` | Array of `{cluster, namespace, name, reason}` for controllers with no observed inventory at all. |
| `clustersNotCovered` | Count of clusters that did not answer. |

The `reason` label — `never-reported`, `hibernated`, `stopped`, or `not-connected` — is derived from
the controller's persisted lifecycle phase. It describes why Varroa has no inventory for that
controller at phase granularity and does not imply live connectivity. `never-reported` can describe a
controller that is unreachable right now.

**Read `coverage` every time you query this surface.** An empty result set is not evidence of
absence while `complete` is `false`. Answering "no controller runs this plugin" on a partially
covered fleet during a CVE response is exactly the moment incompleteness costs most. The frontend
renders partial coverage as a persistent, non-dismissible notice above every result — never as a
footnote that the results can push off screen.

### This release covers the local cluster only (R22)

Remote clusters are reported as not-covered: each gets a `clusters[]` entry with `ok: false` and an
explanatory error, counted in `clustersNotCovered`. Their controllers contribute no rows and appear
in neither `controllersTotal` nor `controllersMissing`. On a multi-cluster install `complete` is
always `false` in v1 — this is a durable scope statement, not a transient error.

**Follow-up tracked: cross-cluster plugin inventory transport.** Required before the advisory-use
case is trustworthy on a multi-cluster fleet. The limitation is recorded, not overlooked.

### Provenance class

Every installed plugin carries a provenance `class` label computed by the operator's
classification subsystem (T2.1). The BFF reads this label **verbatim** — it never computes,
re-derives, ranks, or infers a class. A label added later by the classifier renders without any
frontend change.

Each class label on a drilldown row is qualified by the flags the controller reports alongside it:
`stale`, `degraded`, `truncated`, `optionalEdgesDropped`, `bootstrapApproximate`, and the
cross-check flag `detailStale`. `bootstrapApproximate` is independent of `degraded` — a bootstrap
classification can be approximate without being degraded. The per-controller drilldown behind
`detailPath` remains the full classified view, including fields the fleet surface deliberately omits.

The rollup surface carries a `classes[]` breakdown — one entry per distinct class label across
controllers for that plugin, ordered bytewise (deterministic, not a ranking). This is how the rollup
discloses a plugin that is `declared` on some controllers and unmanaged on others without requiring
the operator to open the drilldown. Class labels are carried as received; the UI holds no hardcoded
list and no ordering assumption (R19).

### Related pages

- [Controllers](../architecture/overview.md) — the per-controller classified sub-resource
- [RBAC](../security/varroa-rbac.md) — fleet plugin results are caller-scoped
