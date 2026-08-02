# Local development (`make localdev`)

<!-- sources: hack/localdev/, charts/varroa/values-localdev.yaml, Makefile -->

One command stands up the **entire Varroa stack on a disposable [kind](https://kind.sigs.k8s.io/)
cluster** — operator, gateway, BFF, frontend, NATS, HTTPS ingress, and a sample Jenkins
controller — reachable from your browser, with nothing pushed to or pulled from a remote Varroa
registry.

```bash
make localdev
```

When it finishes, open **<https://app.varroa.localtest.me>** and log in as
**`admin` / `password`**.

## Prerequisites

| Tool | Notes |
|---|---|
| `docker` | daemon running; builds the backend + frontend images |
| `kind` | cluster lifecycle |
| `kubectl`, `helm` | deploy tooling |
| `git` | repo tooling |
| `mkcert` *(optional)* | browser-trusted local TLS; without it a self-signed cert is generated (browser warning, click through — login still works) |

- **Host ports 80 and 443 must be free** — the kind node maps them so the ingress answers on
  localhost. The preflight check tells you what to stop if they're busy.
- **Internet access** — Jenkins images come from Docker Hub, plugins from the Jenkins update
  center.
- All `*.varroa.localtest.me` hostnames resolve to `127.0.0.1` via public DNS — **no `/etc/hosts`
  edits**.

### WSL2 notes

Run everything (docker, kind, `make localdev`) inside WSL2. The kind port mappings bind
`0.0.0.0`, and Windows forwards `localhost` into WSL2, so a **Windows browser works as-is** at the
same URLs. For warning-free TLS, run `mkcert -install` inside WSL2 **and** import the mkcert root
CA (`mkcert -CAROOT`) into the Windows certificate store — otherwise expect a one-time certificate
warning per host.

## What you get

| URL | What | Credentials |
|---|---|---|
| `https://app.varroa.localtest.me` | Varroa dashboard | `admin` / `password` |
| `https://git.varroa.localtest.me/cgi-bin/git/localdev-bundle.git` | In-cluster git server hosting the sample bundle (clone URL) | — |
| `https://getting-started.varroa.localtest.me` | Sample Jenkins controller | dashboard SSO |

Under the hood: single replicas of every component, PVCs on kind's default `standard`
StorageClass, images built locally and loaded with `kind load docker-image` (tags derive from the
Docker image ID, so unchanged code never rolls pods), and a CoreDNS rewrite so in-cluster pods
resolve `*.varroa.localtest.me` to the ingress. Auth is Varroa's built-in **local mode**: the BFF
is the identity provider, users are `User` CRs (a seeded `admin` user, more via the dashboard's
admin area), and controllers validate session tokens offline against the BFF's JWKS — the full
dashboard→Jenkins SSO path runs, no external IdP. The app host is HTTPS because login cookies are
`Secure`-only.

The sample controller is fed by a fully in-repo bundle (`hack/localdev/bundle/`) served from an
in-cluster smart-HTTP git server (a tiny locally built image: `git-http-backend` behind busybox
httpd) — edit those files and run `make localdev-controller` to publish changes.

## Day-to-day loop

```bash
make localdev-images      # rebuild backend+frontend, kind-load, roll only what changed
make localdev-controller  # (re-)apply the sample controller / bundle changes
make localdev             # full converge — safe to re-run anytime (idempotent)
make localdev-down        # delete the cluster (all in-cluster data, including PVCs)
```

`make localdev-down` keeps `.localdev/` (the generated TLS cert), so browser trust survives
cluster rebuilds. `LOCALDEV_SKIP_CONTROLLER=1 make localdev` skips the sample controller.

### Update Center seeding

When `make localdev` runs, it automatically deploys the [Update Center](../operations/update-center.md) component (`updateCenter.enabled=true`) with a local PVC store and seeds it with a plugin pack:

- **Online (default):** `seed_updatecenter()` imports the CI-published plugin pack from `oci://ghcr.io/varroaci/varroa-jenkins/plugin-pack:jenkins-version-2-555`. If the import fails (e.g. transient network issue), the sample controller still reaches `Connected` via pull-through — the failure is non-fatal.
- **Offline (`LOCALDEV_OFFLINE=1`):** pull-through is disabled and the seed is loaded from a local OCI-layout directory at `hack/localdev/pluginpack-fixture/` via `varroactl import --from dir://...`. A failed import in offline mode is **fatal** (no fallback). The offline flag also implies `LOCALDEV_SKIP_CONTROLLER=1` since there is no network-reachable plugin closure for the sample controller.

To pre-seed the offline fixture, run `bin/varroactl export plugins --profile jenkins-version-2-555 --plugins-file hack/localdev/pluginpack-fixture-plugins.yaml --to dir://hack/localdev/pluginpack-fixture` while you have network access (the `generate.sh` script in the fixture directory automates this).

```bash
LOCALDEV_OFFLINE=1 make localdev
# Update Center seeded from local fixture, controller skipped
```

For pure frontend work you can still run the Vite dev server against the localdev BFF — see the
frontend notes in the repo README (`npm run dev`, port `3000`).

## Known limitations

- **Local auth, not OIDC** — the prod OIDC/Dex code path isn't exercised. Group-based RBAC *is*
  testable: local mode resolves groups from `Group` CRs (add `admin` to a Group's `members`).
- **No observability stack** — prometheus/grafana are disabled and OTLP telemetry export is off.
- **Fixed ports and domain** — 80/443 and `varroa.localtest.me` are not configurable.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Preflight: "host port 80/443 is busy" | Another server (or a stale kind cluster) owns the port. `ss -ltnp 'sport = :443'` to find it, or `make localdev-down`. |
| Browser certificate warning | No mkcert (or its CA not imported on the Windows side). Safe to click through — cookies still work. For clean TLS: install mkcert, delete `.localdev/`, re-run `make localdev`. |
| Jenkins image pulls fail with `toomanyrequests` | Docker Hub anonymous rate limit — repeated cluster rebuilds re-pull `jenkins/jenkins`. `docker login` (any free account) raises the limit. |
| "the sample controller is not ready yet" warning | Usually slow Jenkins image/plugin downloads. Diagnose: `kubectl --context kind-varroa-localdev -n varroa describe controller getting-started` and the operator logs; re-converge later with `make localdev-controller`. |
| Login rejected for `admin` | The admin `User` CR may not be reconciled yet (the operator hashes the seeded password). `kubectl --context kind-varroa-localdev -n varroa-system get user admin -o yaml` — wait for `status.credentials`, or re-run `make localdev`. |
| Jenkins data disappeared | `make localdev-down` deletes PVCs. That's by design — localdev is disposable. |

**Scope note:** the operator runs with `GIT_SSL_NO_VERIFY=true` in localdev only (values
overlay: `charts/varroa/values-localdev.yaml`) so it can clone the sample bundle from the
self-signed in-cluster git server. Don't copy that setting to a real cluster.
