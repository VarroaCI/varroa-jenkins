# Observability

Varroa exposes component metrics and a controller activity feed for your own Prometheus-compatible scraper and log platform.

## Scrape metrics

Metrics cover the operator, gateway, BFF, recorder (when `recordStore.enabled`), and the enabled update center. Set `telemetry.metricsToken` first, then allow the scraper through [Network policies](../install/network-policies.md).

The operator, gateway, BFF, and recorder export `varroa.bus.connected`, tagged with a `component`
attribute: 1 with a NATS connection, 0 without. Alert on a sustained 0. A brief 0 after a credential
rotation clears itself; see [Troubleshooting](troubleshooting.md). `varroa.bus.permission_violations`,
with the same attribute, counts the publishes and subscribes NATS denied. A denied publish returns no
error, so alert on any increase. The recorder also exports `recorder_fence_veto_total`,
`recorder_claims_vetoed_total`, `recorder_claim_veto_marked_total`, and
`recorder_job_home_changed_total`, and logs each refused
claim; see [Build claims](build-claims.md).

The operator exports `varroa.imperative_command.results`, tagged with `kind` (for example
`cancel_build`) and `outcome` (`success`, `failure`, or `deferred`; a deferred restart waits for
running builds). The mite logs each result with the kind, the job path, the run, and the reason:
info on success, warning on failure. A refusal reason is the plugin's plain text or the Jenkins
page title, cut at 300 characters. The gateway logs a warning for a refused cancel, input answer,
or replay.

## Use activity history

Activity history needs the [build record store](build-records.md). Set `recordStore.enabled: true` and size `recordStore.retention.activity` (default `30d`) in chart values.

```bash
varroactl activity --controller <name>
varroactl activity --controller <name> --follow
```

`varroactl activity` prints a backfill table, then, with `--follow`, streams new events live. Without the record store the table is empty and `--follow` shows only new events. The feed records lifecycle and audit events; `varroactl logs <namespace>/<name> --follow` gives controller logs.

Four [cell](cells.md) pipelines events reach the feed only through the store, so a live subscriber sees them on its next backfill. They are `build.interrupted` (an interrupted run), `build.scheduled.backstop` (a missed cron build the backstop fires), `build.claim_expired` (an accepted run with no claimed build number), and `build.claim_vetoed` (a refused claim).

The operator publishes one `command.failed` event for each imperative command that fails on a
controller, with the sender in `actor`, the kind in `reason`, the job path, the run, the HTTP
status, and the error text. A person's successful cancel, input answer, or replay publishes
`command.succeeded` with the same fields; other successes and a deferred restart publish no event.

## Read brood health

The dashboard's brood health card summarizes the brood. A bar shows the number of controllers in each phase. Under the bar, the card lists at most eight controllers. Controllers that need attention come first, then controllers with no live mite that are not hibernated. The rest of the brood is on the Controllers page. Each listed row carries:

| Element | Source |
|---|---|
| Connection dot | Whether the mite currently holds a command stream to the gateway. |
| Phase pill | `status.phase`, plus the needs-attention label when one applies. |
| `seen <age>` | Age of the last mite heartbeat. A controller no mite has ever reported reads `never seen`. |
| Health verdict | The mite's last Jenkins health probe: `healthy`, `unhealthy`, or `unreachable`. |
| `Jenkins <version>` | The running Jenkins version. Absent until a mite reports one. |

Every element is a reported fact. The card holds no history: use the activity feed above for a timeline, and [Troubleshooting](troubleshooting.md) for a controller that reads `never seen`.

## Diagnose gaps

| Symptom | Check |
|---|---|
| Metrics return `401` | Scraper bearer token and chart value. |
| Metrics cannot connect | NetworkPolicy and ServiceMonitor or scrape target. |
| Activity history is missing | `recordStore.enabled` and `VARROA_RECORDSTORE_DSN` on the BFF. |
| Plugin inventory is incomplete | [Update Center](update-center.md) readiness and version profiles. |
| `varroa.bus.connected` is 0 | NATS availability, then [Troubleshooting](troubleshooting.md) for credential rotation. |
| `varroa.bus.permission_violations` increases | The component's NATS grants, in [Troubleshooting](troubleshooting.md). |
| A controller reads `never seen` | Mite registration and gateway connectivity for that controller. |
