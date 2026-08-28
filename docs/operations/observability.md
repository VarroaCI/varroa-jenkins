# Observability

Varroa exposes component metrics and a controller activity feed. Configure your existing Prometheus-compatible scraper and log platform to collect them.

## Scrape metrics

The chart exposes metrics for the operator, gateway, BFF, and enabled update center. Configure `telemetry.metricsToken` before exposing metrics, then allow the scraper through [Network policies](../install/network-policies.md). Keep metrics endpoints private to trusted monitoring workloads.

## Use activity history

Choose activity retention in chart values: `off`, `7d`, `30d`, or `90d`. With retention off, history is available only while the serving BFF remains available. Use a retained setting when audit history must survive restarts.

```bash
varroactl activity --controller <name>
varroactl activity --controller <name> --follow
```

The activity feed records lifecycle and audit events. It is not a substitute for Jenkins build logs. Use `varroactl logs <namespace>/<name> --follow` for controller logs.

## Diagnose gaps

| Symptom | Check |
|---|---|
| Metrics return `401` | Scraper bearer token and chart value. |
| Metrics cannot connect | NetworkPolicy and ServiceMonitor or scrape target. |
| Activity history is missing | Retention setting and BFF availability. |
| Plugin inventory is incomplete | [Update Center](update-center.md) readiness and version profiles. |
