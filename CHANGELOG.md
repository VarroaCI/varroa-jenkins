# Changelog

## [0.3.0](https://github.com/VarroaCI/varroa-jenkins/compare/v0.2.0...v0.3.0) (2026-09-01)


### Features

* in-cluster Jenkins upgrade tracking, promotion, and rollout ([e1325e6](https://github.com/VarroaCI/varroa-jenkins/commit/e1325e63a8054582d30b45578551d2e58ec2dab6))

## [0.2.0](https://github.com/VarroaCI/varroa-jenkins/compare/v0.1.0...v0.2.0) (2026-08-30)


### Features

* dual-license under AGPL-3.0 and commercial terms ([aea64fb](https://github.com/VarroaCI/varroa-jenkins/commit/aea64fb7662c5d10c7ea80a5b5e94de65da53872))


### Bug Fixes

* operator and update-center bug squash ([4484572](https://github.com/VarroaCI/varroa-jenkins/commit/4484572264ba908df5b83b308dae36e416b79d53))

## 0.1.0

Initial public release.

### Update center

- Resolve aged plugin pins by falling back to the artifact-archive `.sha256`
  sidecar when update-center metadata no longer carries the checksum, and
  authenticate seed refs when pulling seed packs from a private registry.
- Include the `/plugins` path segment in the in-cluster update-center download
  URL, so Jenkins can fetch `.hpi` blobs from it.
- Make the archive checksum fallback host configurable
  (`updateCenter.pullThrough.archiveURL`), so an egress-restricted install can
  turn it off.
- Plugin packs now carry the full dependency closure instead of only the
  top-level plugin list.

### Controllers and the operator

- `ControllerSpec` is projected through the API and server-side applies are
  completed from the manager's ownership set. **Breaking**: the API surface for
  controller spec fields changed.
- The operator no longer claims ownership of `Controller` spec fields it does
  not own, so external tools can edit them without being reverted.
- `executeGroovy` targets now fail when the script fails to compile or throws,
  instead of reporting success.
- The gateway retries failed bus watches and surfaces a starved mite rather
  than hanging.
- A superseded mite stream no longer dispatches inbound messages.
- Update-center sync no longer starves bundle recomposition — the two share a
  reconcile queue and long syncs used to block it.

### Dashboard

- State-aware controller detail redesign, organised around
  Bundle → Apply → Jenkins.
- Free-form maps are editable in the curated spec form.
- Named SSE frames are delivered to consumers, and terminal-status streams shut
  down cleanly.
- Readability and alignment passes across the dashboard, filter bar, wizard
  card, and key/value grids.
- The install wizard supplies and validates the dashboard host for path-mode
  ingress.

### MCP tools

- Every mutating MCP tool audited; results sanitised, listings projected to
  summaries, and all tools classified.
- Composed-bundle dry-run tools, OCI blob fetch, and catalog source filtering
  added or fixed.
- Every mutable controller spec field is now reachable from create/update, and
  there is a documented way past a server-side-apply field conflict.

### Chart and packaging

- Released charts pin the component images to the chart version instead of
  tracking `latest`.
- Pod placement controls for operator, gateway, BFF, Dex, and NATS.
- The rolled-in Prometheus and Grafana subcharts were removed — bring your own
  observability stack.

### CLI

- `varroactl` treats a registry port as a port rather than a tag.
