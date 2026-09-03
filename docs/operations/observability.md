# Observability

Varroa exposes component metrics and a controller activity feed. Configure your existing Prometheus-compatible scraper and log platform to collect them.

## Scrape metrics

The chart exposes metrics for the operator, gateway, BFF, and enabled update center. Configure `telemetry.metricsToken` before exposing metrics, then allow the scraper through [Network policies](../install/network-policies.md). Keep metrics endpoints private to trusted monitoring workloads.

The operator, gateway, and BFF export `varroa.bus.connected`, tagged with a `component`
attribute. It reads 1 while the component holds a NATS connection and 0 while it
does not. Alert on a sustained 0. A brief 0 is expected after a credential
rotation and clears itself.

## Use activity history

Choose activity retention in chart values: `off`, `7d`, `30d`, or `90d`. With retention off, history is available only while the serving BFF remains available. Use a retained setting when audit history must survive restarts.

```bash
varroactl activity --controller <name>
varroactl activity --controller <name> --follow
```

The activity feed records lifecycle and audit events. It is not a substitute for Jenkins build logs. Use `varroactl logs <namespace>/<name> --follow` for controller logs.

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
| Activity history is missing | Retention setting and BFF availability. |
| Plugin inventory is incomplete | [Update Center](update-center.md) readiness and version profiles. |
| `varroa.bus.connected` is 0 | NATS availability, then [Troubleshooting](troubleshooting.md) for credential rotation. |
| A controller reads `never seen` | Mite registration and gateway connectivity for that controller. |
