# Observability

Varroa exposes component metrics and a controller activity feed. Configure your existing Prometheus-compatible scraper and log platform to collect them.

## Scrape metrics

The chart exposes metrics for the operator, gateway, BFF, recorder (when `recordStore.enabled`), and enabled update center. Configure `telemetry.metricsToken` before exposing metrics, then allow the scraper through [Network policies](../install/network-policies.md).

The operator, gateway, BFF, and recorder export `varroa.bus.connected`, tagged with a `component`
attribute. It reads 1 while the component holds a NATS connection and 0 while it
does not. Alert on a sustained 0. A brief 0 after a credential rotation clears
itself; see [Troubleshooting](troubleshooting.md). The counter `varroa.bus.permission_violations`, with the
same attribute, counts publishes and subscribes the NATS server denied. A denied publish returns no
error, so alert on any increase. The recorder also exports `recorder_fence_veto_total`,
`recorder_claims_vetoed_total`, `recorder_claim_veto_marked_total`, and
`recorder_job_home_changed_total`, and logs each refused
claim; see [Build claims](build-claims.md).

The operator exports `varroa.imperative_command.results`, tagged with `kind` and `outcome`. It
counts every result of an imperative command that a mite reports, for example `cancel_build` or
`trigger_build`. `outcome` is `success`, `failure`, or `deferred`. A deferred result is a restart
that waits for running builds. The mite also logs each result: at info level on success, and at
warning level on failure. The log line names the kind, the job path, the run, and the reason.

## Use activity history

Activity history needs the [build record store](build-records.md). Set `recordStore.enabled: true` and size `recordStore.retention.activity` (default `30d`) in chart values.

```bash
varroactl activity --controller <name>
varroactl activity --controller <name> --follow
```

`varroactl activity` prints a backfill table, then, with `--follow`, streams new events live. Without the record store, the backfill table is always empty: `--follow` still shows events published after you start watching, but no history from before that moment.

The activity feed records lifecycle and audit events. It is not a substitute for Jenkins build logs. Use `varroactl logs <namespace>/<name> --follow` for controller logs.

Four [cell](cells.md) pipelines events appear in the feed. `build.interrupted` marks an
interrupted run. `build.scheduled.backstop` marks a missed cron build that the backstop fires.
`build.claim_expired` marks an accepted run that never claimed a build number.
`build.claim_vetoed` marks a refused claim. The recorder
writes all four directly to the store. A live subscriber sees each event only on its next
backfill, never on the live stream.

The operator publishes one `command.failed` event for each imperative command that fails on a
controller. A cancel of a build that Jenkins cannot find is an example. The event names the
command kind in `reason`, the job path, the run, and the error text. A successful command and a
deferred restart publish no event.

## Read brood health

The dashboard's brood health card lists every controller with the state Varroa last observed. Each row carries:

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
