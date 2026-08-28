# Changelog

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
