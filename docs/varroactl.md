# varroactl CLI Reference

`varroactl` is the official command-line client for Varroa's REST API. It
manages contexts, authenticates via browser or API key, and provides a
kubectl-style command surface for controllers.

## Install / Build

```bash
make build-cli
```

Produces `bin/varroactl`. The version string is stamped from `git describe`:

```bash
bin/varroactl version --client
```

No external dependencies beyond a Go toolchain matching the project's `go.mod`.

## Quick Start

```bash
# Log in (browser flow — opens your default browser).
varroactl login --server https://varroa.example.com

# List controllers.
varroactl get controllers

# Describe a single controller.
varroactl describe controller team-a/my-controller

# Show who you are.
varroactl whoami
```

## Login

Three login flows are available, all ending with a `vk_` API key stored in a
CLI context.

### Browser loopback (default)

```bash
varroactl login [--server URL] [--context NAME]
```

1. Opens your browser to `https://<server>/cli-auth?port=...&state=...&name=...`
2. The frontend page authenticates you (redirecting to your IdP or showing an
   inline login form).
3. You **Approve** the key request on the page.
4. The token is delivered to the CLI's loopback listener at `127.0.0.1`, stored
   in the context, and the CLI prints the confirmed identity.

**Headless / SSH sessions:** If no browser is available, the CLI prints the
login URL. Open it manually on any machine with a browser, then return to the
terminal. The command waits up to `--timeout` (default 5m).

```bash
# Longer timeout for slow environments.
varroactl login --server https://varroa.example.com --timeout 10m
```

### API key (headless / CI)

```bash
varroactl login --server https://varroa.example.com --api-key vk_...
```

The key is validated against `GET /me` before storing. To avoid shell history:

```bash
# Read the key from stdin (one line).
echo "vk_..." | varroactl login --server https://varroa.example.com --api-key -
```

### Username / password (local / LDAP)

```bash
varroactl login --server https://varroa.example.com --username admin
```

- The server must be in `local` or `ldap` mode (OIDC fails with guidance).
- Password prompts interactively (no echo); use `--password-stdin` for scripts:

```bash
echo "s3cret" | varroactl login --username admin --password-stdin
```

`--api-key` and `--username` are mutually exclusive.

## Contexts & Configuration

The CLI stores server and credential information in contexts, similar to
`kubectl` contexts.

### Config File Location

| OS | Path (first hit wins) |
|----|-----------------------|
| Any | `$VARROACTL_CONFIG` (explicit override) |
| Windows | `%AppData%\varroactl\config.yaml` |
| Linux / macOS | `$XDG_CONFIG_HOME/varroactl/config.yaml` |
| Linux / macOS | `~/.config/varroactl/config.yaml` |

### Schema

```yaml
currentContext: prod
contexts:
  - name: prod
    server: https://varroa.example.com
    apiKey: vk_abc123.def456
    defaultNamespace: team-a
    defaultCluster: core
```

The `defaultCluster` field is optional — old config files without it load
unchanged. Cluster resolution precedence: `--cluster` flag > config's
`defaultCluster` > `"core"`.

File is written 0600, parent directory 0700.

### Environment Variables

| Variable | Precedence | Purpose |
|----------|------------|---------|
| `VARROACTL_SERVER` | After flags, before config | Server URL override |
| `VARROACTL_API_KEY` | After flags, before config | API key override |
| `VARROACTL_CONTEXT` | After flags, before config | Context name override |
| `VARROACTL_CONFIG` | Direct override | Explicit config file path |

Resolution order: **flags > environment variables > config file**.

### Config Subcommands

| Command | Description |
|---------|-------------|
| `varroactl config get-contexts` | List contexts (CURRENT, NAME, SERVER, NAMESPACE, CLUSTER — never the key) |
| `varroactl config current-context` | Print the active context name |
| `varroactl config use-context NAME` | Switch to an existing context |
| `varroactl config set-context NAME [--server] [--api-key] [--namespace] [--cluster]` | Create or update; only provided fields overwrite |
| `varroactl config set-cluster NAME\|--unset` | Set or clear the default cluster on the current context |
| `varroactl config delete-context NAME` | Delete a context (clears currentContext if it pointed there) |

## Resources

The CLI manages resource types beyond controllers. This section covers every
resource family, its cluster-scoped or namespaced addressing, and the CRUD
conventions.

### Addressing Conventions

| Scope | Example | Notes |
|-------|---------|-------|
| **Namespaced** | `NAMESPACE/NAME` or `NAME -n NAMESPACE` | Controllers, catalogsources, catalogitems, composedbundles, provisioningdefaults |
| **Cluster-scoped** | bare `NAME` | Roles, rolebindings, jenkinsroles, jenkinsrolebindings. An explicit `-n` is a usage error (exit 2). |
| **No detail/delete** | groups, users, builtin-roles, identity-settings, deployable-namespaces | Detail endpoint absent; `get group NAME`, `edit group` are **not** registered. |

### Full-Replace Edit Semantics

`edit <NOUN> <NAME>` opens a temporary file in `$EDITOR`. On save:

1. The buffer is seeded from the detail endpoint (or from the list entry when
   no detail endpoint exists).
2. **Only writable fields** are shown — status, conditions, and
   `observedGeneration` are stripped before display.
3. On save the CLI sends a **full-replace PUT** of the entire buffer.
   Omitting a field in the editor clears it server-side.
4. The controller's `namespace` (namespaced resources) or `name` is determined
   by the path segment; changing `name` in the editor renames the resource
   server-side.

### RBAC — Roles, RoleBindings, JenkinsRoles, JenkinsRoleBindings

Four cluster-scoped CRD types with identical CRUD surfaces. Aliases shown
below; each family has `get`, `create`, `edit`, `delete`.

| Command | Endpoint Effect | Example |
|---------|----------------|---------|
| `get roles` | `GET /roles` | `varroactl get roles` |
| `get role NAME` | `GET /roles/{name}` | `varroactl get role admin` |
| `create role -f FILE\|-` | `POST /roles` (CR body) | `varroactl create role -f my-role.yaml` |
| `edit role NAME` | GET → `$EDITOR` → `PUT /roles/{name}` | `varroactl edit role admin` |
| `delete role NAME` | `DELETE /roles/{name}` → 204 | `varroactl delete role admin` |

Same pattern for `rolebinding(s)` (`rb`), `jenkinsrole(s)` (`jr`),
`jenkinsrolebinding(s)` (`jrb`):

| Command | Resource | Path |
|---------|----------|------|
| `get rolebindings` | VarroaRoleBinding | `/rolebindings` |
| `get jenkinsroles` | JenkinsRole | `/jenkinsroles` |
| `get jenkinsrolebindings` | JenkinsRoleBinding | `/jenkinsrolebindings` |

### Teams, Groups, Users

| Command | Endpoint Effect | Example |
|---------|----------------|---------|
| `get teams` | `GET /teams` | `varroactl get teams` |
| `get team NAME` | `GET /teams/{name}` | `varroactl get team my-team` |
| `create team -f FILE\|-` | `POST /teams` | `varroactl create team -f team.yaml` |
| `edit team NAME` | GET → `$EDITOR` → `PUT /teams/{name}` | `varroactl edit team my-team` |
| `delete team NAME` | `DELETE /teams/{name}` → 204 | `varroactl delete team my-team` |
| `get groups` | `GET /groups` | `varroactl get groups` |
| `create group -f FILE\|-` | `POST /groups` (local/LDAP only) | `varroactl create group -f group.yaml` |
| `delete group NAME` | `DELETE /groups/{name}` → 204 | `varroactl delete group my-group` |
| `get users` | `GET /users` | `varroactl get users` |
| `create user -f FILE\|- [--password-stdin]` | `POST /users` | `varroactl create user -f user.yaml --password-stdin` |
| `edit user NAME` | GET list → `$EDITOR` → `PUT /users/{name}` | `varroactl edit user jane` |
| `delete user NAME` | `DELETE /users/{name}` → 204 | `varroactl delete user jane` |

There is **no** `GET /users/{name}` or `GET /groups/{name}` endpoint — the
`edit user` buffer is seeded from the list entry. `get group NAME` and `edit
group` are **not** registered.

### Catalog Sources

| Command | Endpoint Effect | Example |
|---------|----------------|---------|
| `get catalogsources [-n NS]` | `GET /catalogsources[?namespace=]` | `varroactl get catalogsources -n team-a` |
| `get catalogsource NS/NAME` | `GET /catalogsources/{ns}/{name}` | `varroactl get catalogsource team-a/my-src` |
| `create catalogsource -f FILE\|- [-n NS]` | `POST /catalogsources/{ns}` | `varroactl create catalogsource -n team-a -f src.yaml` |
| `edit catalogsource NS/NAME` | GET → `$EDITOR` → `PUT` | `varroactl edit catalogsource team-a/my-src` |
| `delete catalogsource NS/NAME` | `DELETE /catalogsources/{ns}/{name}` → 204 | `varroactl delete catalogsource team-a/my-src` |
| `sync catalogsource NS/NAME` | `POST /catalogsources/{ns}/{name}/sync` | `varroactl sync catalogsource team-a/my-src` |

`catalogsource` also accepts the alias `cs`.

### Catalog Items

Read-only (no create/edit/delete).

| Command | Endpoint Effect | Example |
|---------|----------------|---------|
| `get catalogitems [-n NS] [--source S] [--type T] [--query Q]` | `GET /catalogitems?namespace=&source=&type=&q=` | `varroactl get catalogitems --source my-src --type plugin` |
| `get catalogitem NS/NAME` | `GET /catalogitems/{ns}/{name}` | `varroactl get catalogitem team-a/test-item` |

The list response includes summary items only (`name`, `namespace`, type,
version, validity, and related metadata) plus an `operatorNamespace` field
visible in `-o json` output. The detail response is the full `CatalogItem` CR
(spec + status incl. `status.content`).

### Composed Bundles

| Command | Endpoint Effect | Example |
|---------|----------------|---------|
| `get composedbundles [-n NS]` | `GET /composedbundles[?namespace=]` | `varroactl get bundles` |
| `get composedbundle NS/NAME` | `GET /composedbundles/{ns}/{name}` | `varroactl get bundle team-a/app-bundle` |
| `create composedbundle -f FILE\|- [-n NS]` | `POST /composedbundles/{ns}` | `varroactl create bundle -n team-a -f bundle.yaml` |
| `edit composedbundle NS/NAME` | GET → `$EDITOR` → `PUT` | `varroactl edit bundle team-a/app-bundle` |
| `delete composedbundle NS/NAME` | `DELETE .../{ns}/{name}` → 204 | `varroactl delete bundle team-a/app-bundle` |
| `validate bundle -f FILE\|- [-n NS]` | `POST /composedbundles/validate?namespace=NS` | `varroactl validate bundle -n team-a -f bundle.yaml` |
| `preview bundle -f FILE\|- NS/NAME` | `POST /composedbundles/{ns}/preview` | `varroactl preview bundle -f overlay.yaml team-a/app-bundle` |
| `pause bundle NS/NAME` | `POST /composedbundles/{ns}/{name}/pause` | `varroactl pause bundle team-a/app-bundle` |
| `resume bundle NS/NAME` | `POST /composedbundles/{ns}/{name}/resume` | `varroactl resume bundle team-a/app-bundle` |

Aliases: `bundle(s)`, `cb`, `composedbundle(s)`.

**Validate spec extraction:** If the file contains a CR root (`apiVersion`,
`kind`, or `metadata`) the CLI sends `.spec`; otherwise it sends the document
as-is (a bare `ComposedBundleSpec`). Namespace is passed as a query parameter
(omitted → server defaults to `default`; use `-n` since itemRefs resolve in
that namespace).

### Provisioning Defaults & Version Profiles

| Command | Endpoint Effect | Example |
|---------|----------------|---------|
| `get provisioningdefaults [--cluster CLUSTER]` | `GET /clusters/{cluster}/provisioningdefaults/varroa-defaults` (NAME defaults to the `varroa-defaults` singleton) | `varroactl get provisioningdefaults --cluster prod` |
| `get provisioningdefaults NAME [--cluster CLUSTER]` | `GET /clusters/{cluster}/provisioningdefaults/{name}` | `varroactl get provisioningdefaults varroa-defaults --cluster prod` |
| `edit provisioningdefaults NAME [--cluster CLUSTER]` | `GET /clusters/{cluster}/provisioningdefaults/{name}` → `$EDITOR` → `PUT` | `varroactl edit provisioningdefaults varroa-defaults --cluster prod` |
| `get versionprofiles [--cluster CLUSTER]` | `GET /clusters/{cluster}/version-profiles` | `varroactl get versionprofiles --cluster prod` |
| `get versionprofiles NAME [--cluster CLUSTER]` | `GET /clusters/{cluster}/version-profiles/{name}` | `varroactl get versionprofiles jenkins-version-2-555 --cluster prod` |

`provisioningdefaults` is a cluster-scoped singleton resource with a built-in
name `varroa-defaults`. `versionprofiles` is cluster-scoped. Both accept
`--cluster` and default to the active context's `defaultCluster`, then `core`.

### Provisioning Config & Deployable Namespaces (Singletons)

| Command | Endpoint Effect | Example |
|---------|----------------|---------|
| `get provisioning-config [--cluster CLUSTER]` | `GET /clusters/{cluster}/provisioning/config` | `varroactl get provisioning-config --cluster prod` |
| `get deployable-namespaces` | `GET /clusters/{cluster}/namespaces/deployable` | `varroactl get deployable-namespaces` (cluster core; use `--cluster` for others) |
| `get identity-settings` | `GET /identity-settings` | `varroactl get identity-settings` |
| `get builtin-roles` | `GET /builtin-roles` | `varroactl get builtin-roles` |

These are cluster-scoped singletons — no name argument, no `--namespace` or
`-n` flag. Use `-o json` or `-o yaml` to inspect the full payload, or
`describe` for a human-readable summary.

Every resource with a detail GET registers `describe <NOUN> <NAME>`.

## API Keys & Passwords

### API Keys (`varroactl apikey`)

Manage your `vk_`-prefixed API keys. All key operations are **self-only**
except where noted with `--user` (admin).

| Command | Description | Example |
|---------|-------------|---------|
| `apikey list [--user U]` | List keys (self, or a user as admin) | `varroactl apikey list` |
| `apikey create [NAME] [--expires-in DUR]` | Create a new key; prints the token to stdout | `varroactl apikey create ci-key --expires-in 720h` |
| `apikey revoke PREFIX [--user U]` | Revoke a key by its 7-character prefix | `varroactl apikey revoke vk_abc123` |
| `apikey rotate PREFIX [--expires-in DUR] [--name N]` | Rotate a key (new token, same prefix) | `varroactl apikey rotate vk_abc123 --expires-in 720h` |

The `create` command prints the raw `vk_...` token to **stdout** for scripting,
and any warning (e.g. about an expiring current key) to stderr.

#### `--expires-in` Duration

Accepts a **Go duration string** — `"720h"` (30 days), `"168h"` (7 days),
`"8760h"` (1 year). Validated client-side; invalid values produce a usage
error (exit 2). Pass the string through verbatim to the server.

#### Rotate Partial-Failure

On success the new token prints to stdout. In rare cases the server may
return HTTP 500 with both an `error` message and a `newToken` (the new key is
live server-side but the old one could not be cleaned up). The CLI prints the
error to stderr, the `newToken` to stdout, and exits 1. Check the output
carefully — the new key is valid.

### Passwords (`varroactl passwd`)

| Form | Description | Example |
|------|-------------|---------|
| `passwd` | Change your own password. Prompts for old, then new (×2) via `golang.org/x/term` (no echo). Sends `{oldPassword, newPassword}`. | `varroactl passwd` |
| `passwd USER` | Admin: set another user's password. Prompts for new (×2). Sends `{newPassword}`. | `varroactl passwd jane` |
| `passwd --password-stdin` | Read the new password from stdin (no confirmation prompt). Self mode (`passwd --password-stdin`) still prompts for the old password on the TTY. | `echo "s3cret" \| varroactl passwd jane --password-stdin` |

Mode-gated: these commands return 405/501 outside `local` or `ldap` auth
mode.

## Brood Operations

The `broodop` noun (aliases `broodops`, `fo`) manages bulk controller operations.
Cross-cluster runs use `--clusters` (selector mode) or cluster-qualified names
(multi-cluster names mode).

```bash
# Preview a restart without creating
varroactl broodop run restart -l tier=canary --filter phase=Connected --dry-run

# Create with explicit clusters (selector mode)
varroactl broodop run restart -l tier=canary --clusters core,dev-cluster

# Create targeting all known clusters
varroactl broodop run reconcile -l app=smoke --clusters all

# Create with cluster-qualified names (multi-cluster names mode)
varroactl broodop run restart --names core/team-a/ctrl-a,dev-cluster/team-b/ctrl-b

# Create and watch
varroactl broodop run stop --names api,web -n teams-payments -w

# List runs (CLUSTERS column shows member clusters)
varroactl broodop get

# List runs filtered to one cluster
varroactl broodop get --cluster core

# Describe a run (shows per-cluster sections with per-target state table)
varroactl broodop describe team-a/broodop-restart-abc12

# Suspend / resume
varroactl broodop suspend team-a/broodop-restart-abc12
varroactl broodop suspend team-a/broodop-restart-abc12 --off

# Cancel
varroactl broodop cancel team-a/broodop-restart-abc12

# Watch live per-cluster updates via SSE (exits 0 on aggregated Succeeded,
# 1 on aggregated Failed/Canceled; stops when a terminal phase is received)
varroactl broodop watch team-a/broodop-restart-abc12
```

`Suspended` is non-terminal, so `broodop watch` continues watching while an
operation is suspended. Terminal phases are `Succeeded`, `Failed`, and
`Canceled` only. Reconcile targets that are Hibernated or Stopped are skipped
with an explanatory reason; an operation with only skipped targets succeeds.
The server closes the watch stream as soon as the run reaches a terminal
phase, even if some or all target clusters are currently unreachable; if the
run never reaches a terminal phase, the server bounds the stream to **1
hour** and closes it with an informative status instead of hanging forever
— `broodop watch` surfaces that message and exits non-zero.
Scheduled operations default to a one-day (`86400` second) operation TTL. Use
the schedule template's `ttlSecondsAfterFinished` or `--ttl=<seconds>` to
override it; `--ttl=0` keeps the completed operation forever.

**Multi-cluster targeting details:**

| Flag | Mode | Description |
|---|---|---|
| `--clusters c1,c2` or `--clusters all` | Selector mode | Fans the identical spec out to every listed cluster |
| `--names core/ns/a,dev-cluster/ns/b` | Names mode, multi-cluster | 3-token `cluster/ns/name` entries are partitioned per cluster (no `--clusters` flag) |

Client-side errors (exit 2, no request sent): mixing qualified and unqualified
`--names`; `--clusters` with any 3-token name; `--clusters all` with `--names`;
multiple clusters with unqualified names.

**List cluster filter:** `--cluster` on `broodop get` is a standalone filter that
maps to `?cluster=`. It does NOT consult the context's `defaultCluster` — brood
runs are cross-cluster aggregates by nature, and a config default silently
narrowing them would be surprising.

See [Brood operations](operations/brood-operations.md) for detailed reference.

## Streaming

Varroa provides several streaming endpoints for real-time controller
observability. All streaming commands install a SIGINT/SIGTERM handler and
exit 0 on interrupt.

### Logs (`varroactl logs NS/NAME -f`)

One-shot (`varroactl logs NS/NAME`) shows the current log tail. With
`--follow` / `-f` it connects to the server's SSE log stream:

```bash
varroactl logs team-a/my-ctrl -f
```

Each line: `<timestamp> <message>`. On reconnect the server re-sends its
recent tail — the CLI suppresses entries with `timestamp ≤ last printed`
(best-effort dedupe; identical-timestamp duplicates are possible).

### Events (`varroactl events controller NS/NAME`)

Streams brood-record events for a single controller:

```bash
varroactl events controller team-a/my-ctrl
```

Default output: one line per event (`<local time>  <event>  <remaining
fields>`). `-o json` emits raw NDJSON frames.

### Mite (`varroactl mite NS/NAME`)

Streams mite lifecycle events (connected, disconnected, heartbeat, snapshot,
init):

```bash
varroactl mite team-a/my-ctrl
```

Default output: `<local time>  <event name>  <compact JSON data>`.
`-o json` emits `{"event":..., "data":...}` NDJSON.

### Activity (`varroactl activity`)

Shows the activity feed — a backfill of recent events, optionally followed by
the live stream.

```bash
# One-shot backfill (up to 200 events).
varroactl activity

# Filter to a single controller (bare name, no namespace).
varroactl activity --controller my-ctrl

# Backfill + follow live stream.
varroactl activity -f --controller my-ctrl
```

Backfill columns: `TIME  TYPE  SOURCE  CONTROLLER  MESSAGE`. With `-f`,
events append in the same columns (no re-header).

**Known window:** Events landing between the backfill GET and the stream
connect are missed until the next reconnect-refetch (the server has no cursor
for seamless catch-up).

### Watch / Get Controllers -w

Two equivalent forms:

```bash
varroactl watch [-n NS]
varroactl get controller -w [-n NS]
```

The watch command:

1. Renders a full controller table (columns: `CLUSTER NAMESPACE NAME PHASE VERSION MITE HEALTH`) from `GET /controllers[?namespace=&cluster=]`.
2. Subscribes to the brood stream via `GET /stream/brood`.
3. On any event it arms a **2-second debounce timer**; when it fires it
   re-fetches the list and prints only rows that are new or changed.
   Controllers that disappeared render one final row with PHASE `Deleted`.
4. On stream reconnect it immediately re-lists (missed-event recovery).

`-o json|yaml|name` combined with `-w` is a usage error (exit 2) — watch
renders tables only.

**Reconnect behavior (all streams):** The client uses exponential backoff
(initial 1s, factor 2, cap 30s, reset on successful open). Terminal HTTP
errors (401/403/404) are returned immediately — a bad token never fixes
itself. A 90-second read deadline (3 missed server keepalives) detects
half-open TCP connections and triggers the reconnect path.

## Search

```bash
varroactl search QUERY
```

Searches the controllers, namespaces, groups, and catalog items the caller is
authorized to view. Results are rendered as `TYPE  CLUSTER  NAMESPACE  NAME`;
non-applicable cluster and namespace cells are blank. The query is passed as
`?q=` to `GET /search`.

```bash
varroactl search my-plugin
```

## MCP

`varroactl mcp` implements the [Model Context Protocol](https://modelcontextprotocol.io/)
(MCP) transport, enabling AI assistants (Claude Code, Claude Desktop) to
interact with the Varroa API.

When an MCP client uses `call_jenkins_tool`, the call executes in Jenkins as
the calling identity, not as a shared Varroa system user. Varroa checks
controller visibility, then Jenkins applies the caller's federated
permissions. See [MCP and Jenkins controller tools](operations/mcp.md) for
the token boundary, required RBAC, and limitations.

### Stdio Bridge (`varroactl mcp`)

Default mode: reads one JSON-RPC message per line from stdin, forwards it to
`POST /api/v1/mcp`, and writes the response to stdout.

```bash
# Run with a configured context.
varroactl mcp

# Explicit context for the MCP server.
varroactl mcp --context prod
```

- JSON-RPC responses are written to **stdout**.
- All diagnostics (warnings, errors, log messages) go to **stderr**.
- The HTTP client has **no timeout** — tool calls can be long-running.
- SIGINT / stdin EOF drains in-flight requests and exits 0.
- The bridge depends on the BFF's **stateless MCP mode** (`WithStateLess`).
  If the BFF ever becomes stateful this contract breaks.

### MCP Serve (`varroactl mcp serve`)

HTTP reverse proxy mode for clients that cannot run a subprocess:

```bash
# Bind to an ephemeral loopback port.
varroactl mcp serve
# → MCP proxy listening on http://127.0.0.1:34567

# Bind to a specific address (loopback only).
varroactl mcp serve --listen 127.0.0.1:8811
```

- Rewrites every request to `<server>/api/v1/mcp`, injecting the context's
  `Authorization: Bearer` header.
- Strips any inbound `Authorization` or `Cookie` headers (credential
  isolation).
- `--listen` defaults to `127.0.0.1:0` (ephemeral). The address **must**
  resolve to a loopback address (`127.0.0.1`, `::1`, `localhost`) — anything
  else is a usage error (exit 2).
- Runs until SIGINT/SIGTERM, exit 0.

### Security Posture

- Credentials are injected locally — the MCP proxy never exposes the API key
  to network clients.
- The HTTP server binds to loopback only (enforced).
- Stdio mode passes credentials through stdin → BFF with no disk write.
- SSE-style responses stream through unbuffered (`FlushInterval: -1`).

### Client Configuration

**Claude Code:**

```bash
claude mcp add varroa -- varroactl mcp
# With an explicit context:
claude mcp add varroa -- varroactl mcp --context prod
```

**Claude Desktop (`claude_desktop_config.json`):**

```json
{
  "mcpServers": {
    "varroa": {
      "command": "varroactl",
      "args": ["mcp"]
    }
  }
}
```

**HTTP-transport clients:**

```bash
# Start the proxy, then point the client at the printed URL.
varroactl mcp serve --listen 127.0.0.1:8811
```

Configure the MCP client to use `http://127.0.0.1:8811/` as its server URL.

## Controllers Command Reference

All controller commands accept the noun as `controller`, `controllers`, or
`ctrl`.

| Command | What it does | Example |
|---------|-------------|---------|
| `get controller [-n NS \| -A] [--cluster C] [--all-clusters]` | List controllers in a namespace or all namespaces | `varroactl get controllers -n team-a` |
| `get controller NS/NAME [--cluster C]` | Get a single controller detail | `varroactl get controller team-a/my-ctrl` |
| `get controller NS/NAME -o yaml [--cluster C]` | Get server-rendered CR YAML (raw output) | `varroactl get controller team-a/my-ctrl -o yaml` |
| `describe controller NS/NAME [--cluster C]` | Human-friendly controller details | `varroactl describe controller team-a/my-ctrl` |
| `create controller -f FILE [-n NS] [--cluster C]` | Create a controller from a YAML file | `varroactl create controller -n team-a -f my-ctrl.yaml` |
| `delete controller NS/NAME [--cluster C]` | Delete a controller | `varroactl delete controller team-a/my-ctrl` |
| `edit controller NS/NAME [--cluster C]` | Edit a controller in `$EDITOR` | `varroactl edit controller team-a/my-ctrl` |
| `patch controller NS/NAME -p JSON [--cluster C]` | JSON merge patch a controller | `varroactl patch controller team-a/my-ctrl -p '{"spec":{"version":"2.0"}}'` |
| `restart controller NS/NAME [--cluster C]` | Restart (deletes the Jenkins pod) | `varroactl restart controller team-a/my-ctrl` |
| `reprovision controller NS/NAME [--cluster C]` | Reprovision a controller | `varroactl reprovision controller team-a/my-ctrl` |
| `reconcile controller NS/NAME [--cluster C]` | Trigger reconciliation | `varroactl reconcile controller team-a/my-ctrl` |
| `approve controller NS/NAME [--action A] [--cluster C]` | Approve a pending action (`reload`, `restart`, `approve`, `force`, `force-restart`, `plugin-roll`) | `varroactl approve controller team-a/my-ctrl --action restart` |
| `approve controller NS/NAME --deletion PATH [--cluster C]` | Approve a pending deletion | `varroactl approve controller team-a/my-ctrl --deletion "items/some-job.xml"` |
| `diff controller NS/NAME [--cluster C]` | Show config diff (incoming vs applied) | `varroactl diff controller team-a/my-ctrl` |
| `preflight -n NS -f FILE [--cluster C]` | Validate a controller YAML without creating | `varroactl preflight -n team-a -f my-ctrl.yaml` |
| `render -n NS -f FILE [--cluster C]` | Render the server-computed controller YAML | `varroactl render -n team-a -f my-ctrl.yaml` |
| `preview controller NS/NAME -f OVERLAY [--cluster C]` | Preview an overlay before applying | `varroactl preview controller team-a/my-ctrl -f overlay.yaml` |
| `logs NS/NAME [--cluster C]` | Show controller logs (one-shot) | `varroactl logs team-a/my-ctrl` |

## Output Formats

All `get` and action commands support `-o`:

| Format | Description |
|--------|-------------|
| `table` (default) | Tabular view via `text/tabwriter`. List columns: `CLUSTER NAMESPACE NAME PHASE VERSION MITE HEALTH`; `-o wide` adds `ENDPOINT BUNDLE ROUTING JENKINS-VERSION` |
| `json` | Marshal the response DTO as JSON |
| `yaml` | Marshal the response DTO as YAML (except single-controller `get` — see below) |
| `name` | One `<namespace>/<name>` per line (may repeat across clusters) |

**Asymmetry:** `get controller <single> -o yaml` fetches the **server-rendered
Custom Resource YAML** (`GET …/clusters/{cluster}/controllers/{ns}/{name}/yaml` → `200
application/yaml`) and prints it verbatim. Every other `-o yaml` or `-o json`
rendering is a client-side marshal of the response DTO.

Suppress table headers with `--no-headers`.

## Exit Codes

| Code | Meaning |
|------|---------|
| `0` | Success |
| `1` | API or application error (non-2xx response, network failure, validation) |
| `2` | Usage error (unknown flag/command, missing arguments, `-n` with `-A`, invalid `-o` value) |

## Multi-cluster

When the server aggregates multiple clusters, all controller commands accept a
`--cluster` flag to target a specific cluster. Resolution precedence:
`--cluster` flag > `config set-cluster` default > `"core"`.

### Cluster resolution

| Input | Result |
|-------|--------|
| `--cluster dev-cluster` | Targets cluster `dev-cluster` |
| No flag, config default `staging` | Targets `staging` |
| No flag, no config default | Targets `core` (single-cluster default) |

### List semantics

- **Default (no flag, no config default):** unfiltered aggregate — shows
  controllers from all clusters. The `CLUSTER` column identifies each row's
  origin.
- **`--cluster dev-cluster`:** lists only controllers in `dev-cluster`.
- **`--all-clusters`:** forces an unfiltered aggregate even when a config
  default exists. Mutually exclusive with `--cluster`.
- **Config `defaultCluster`:** acts as a persistent `--cluster` filter on
  list commands.

### Clusters noun

The `get clusters` / `get cluster NAME` command lists registered clusters:

```text
NAME         STATUS     STATE      CONTROLLERS   CONNECTED   LAST-HEARTBEAT
core         Healthy    Active     47            42          2025-04-11T12:00:00Z
dev-cluster  Healthy    Draining   5             4           2025-04-11T12:00:00Z
remote       Unhealthy  Drained    0             0           2025-04-11T10:15:00Z
```

Wide columns (`-o wide`): `OPERATOR-VERSION`, `K8S-VERSION`.

`-n` and `-A` are rejected (exit 2) — clusters are cluster-scoped.

### `-o name` ambiguity

When the same namespace/name exists in two clusters, `-o name` repeats the
line. Scripts needing uniqueness should use `--cluster` or `-o json`.

### D6 surface note

Log tailing and non-controller resources (teams, RBAC, API keys, etc.) are
core-cluster-local. Targeting them with `--cluster` on a hive returns
the server's error.

---

## Drain

Drain a cluster to decommission it. Admin-only.

```bash
# Drain a cluster (interactive — prompts for cluster name confirmation):
varroactl drain cluster dev-cluster

# Drain without prompt (scripts):
varroactl drain cluster dev-cluster --yes

# Cancel a drain / rejoin a drained cluster:
varroactl drain cluster dev-cluster --cancel
```

Drain deletes every Controller CR on the cluster (StatefulSets, Services,
Ingresses torn down). Jenkins data is **not migrated** — see the
[drain documentation](install/multi-cluster.md#6-draining-and-decommissioning-a-cluster)
for details.

- Exit 0 on success; prints the resulting state (`draining`, `drained`, `active`).
- Exit 1 on server error (403 for non-admin, 409 for cancel on an active cluster, etc.).
- Exit 2 on usage error (non-TTY without `--yes`).

The `cancel` output also prints a note to stderr that controllers already
deleting are not restored by canceling.

---

## Logout & Key Lifecycle

```bash
# Deactivate the key from the active context (keeps the key valid server-side).
varroactl logout

# Deactivate and revoke the key server-side.
varroactl logout --revoke
```

**Default behaviour:** `logout` clears the `apiKey` from the active context
**without** revoking it server-side. This prevents accidentally breaking CI/CD
pipelines that share the same key. The CLI prints the key prefix so you can
revoke later:

```text
API key <prefix> remains valid — revoke with "varroactl logout --revoke"
or from the dashboard.
```

When `--revoke` is used, the CLI calls `DELETE /me/apikeys/{prefix}` and
clears the context only on success. 404 from a previously-revoked key is
treated as success (the key is gone).

You can also manage keys from the profile menu's dedicated **API Keys** page in the Varroa
dashboard at any time.

---

## Plugin Packs

### `export plugins`

Build and publish an OCI plugin pack.

```text
varroactl export plugins --profile <name> --to <dest> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--profile` | _(required)_ | Version profile name (e.g. `2-555`) |
| `--plugins-file` | `""` | Offline plugin lock file in the `core:`/`plugins:` YAML shape (from `hack/gen-plugin-lock.sh`) |
| `--to` | _(required)_ | Destination URI: `oci://<registry>/<repo>[:<tag>]`, `dir://<path>`, or `tar://<path>.tar.gz` |
| `--download-url-base` | `https://updates.jenkins.io` | Base URL for live plugin downloads (used only when `--plugins-file` is absent) |
| `--registry-config` | `""` | Path to Docker `config.json` for registry authentication |
| `--insecure` | `false` | Use plain HTTP for the OCI registry |
| `-o` / `--output` | `""` | Only `json` is accepted; prints `{"digest","pluginCount","ref"}` |
| `--cluster` | context default | Cluster for profile resolution |

**Exits:**

- **0** on success (pack built and pushed).
- **1** on build/network/auth failure (stderr explains).
- **2** on usage error (missing required flag, invalid scheme, unsupported `-o` value).

**`-o json` output shape:**

```json
{
  "digest": "sha256:abc123...",
  "pluginCount": 47,
  "ref": "2-555"
}
```

### `export catalog`

Fetch a CatalogSource from the cluster and publish it as an OCI artifact.

```text
varroactl export catalog --namespace <ns> --name <catalogsource> --to <dest> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--namespace` / `-n` | _(required)_ | Namespace of the CatalogSource |
| `--name` | _(required)_ | Name of the CatalogSource |
| `--to` | _(required)_ | Destination URI (see scheme table in `export plugins` above) |
| `--registry-config` | `""` | Path to Docker `config.json` for registry authentication |
| `--insecure` | `false` | Use plain HTTP for the OCI registry |
| `--cluster` | context default | Cluster for BFF resource lookup |

The materialized catalog is pushed as an OCI artifact with layer media type `application/vnd.varroa.catalog.v1.tar+gzip`.

### `export bundle`

Clone a bundle repository and publish it as an OCI artifact.

```text
varroactl export bundle --repo <url> --path <path> --revision <revision> --to <dest> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--repo` | _(required)_ | Repository URL (`https://`, `ssh://`, or scp-style) |
| `--path` | `"."` | Bundle directory path within the repo |
| `--revision` | `""` | Branch, tag, or commit SHA |
| `--to` | _(required)_ | Destination URI (see scheme table in `export plugins` above) |
| `--registry-config` | `""` | Path to Docker `config.json` for registry authentication |
| `--insecure` | `false` | Use plain HTTP for the OCI registry |
| `-o` / `--output` | `""` | Only `json` is accepted; prints `{"digest","ref"}` |

The materialized bundle is pushed as an OCI artifact with layer media type `application/vnd.varroa.bundle.v1.tar+gzip`.

**`-o json` output shape:**

```json
{
  "digest": "sha256:abc123...",
  "ref": "casc-bundles"
}
```

**Scheme grammar:**

| Scheme | Example | Description |
|--------|---------|-------------|
| `oci://` | `oci://ghcr.io/myorg/plugin-pack:2-555` | Push to an OCI registry |
| `dir://` | `dir:///tmp/plugin-pack` | Write to an OCI-layout directory (created if absent) |
| `tar://` | `tar:///tmp/plugin-pack.tar.gz` | Write a gzipped tar archive |
| `uc://` | — | Recognised syntactically, **not functional** in this build |

**Dual-tag strategy:** When `--to oci://...` has no explicit `:<tag>`, two tags are written — a floating `<profile>` tag and an immutable `<profile>-<lockHash12>` tag. When an explicit tag is given (as CI does), only that single tag is written.

For the full artifact format (media types, annotations, config fields), see [Plugin Packs](config/plugin-packs.md).

### `import`

Copy a plugin pack between OCI-compatible destinations.

```text
varroactl import --from <src> --to <dest> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--from` | _(required)_ | Source URI (`oci://`, `dir://`, `tar://`) |
| `--to` | _(required)_ | Destination URI (`oci://`, `dir://`, `tar://`) |
| `--registry-config` | `""` | Path to Docker `config.json` for registry auth |
| `--insecure` | `false` | Use plain HTTP for the OCI registry |
| `-o` / `--output` | `""` | Only `json` is accepted; prints `{"digest","ref"}` |

**Exits:**

- **0** on success (pack copied).
- **1** on copy/network/auth failure (stderr explains). Also returned when `uc://` is used on either side.
- **2** on usage error (missing required flag, invalid scheme, unsupported `-o` value).

**`-o json` output shape:**

```json
{
  "digest": "sha256:abc123...",
  "ref": "2-555"
}
```

**`uc://` behavior:** The `uc://` scheme is recognised syntactically for forward-compatibility but returns exit 1 with the message `uc:// requires the update-center service (not available in this build)`. This applies to both `--from` and `--to`.

---

← [CLI tutorial](tutorials/varroactl-cli.md) | [Home](README.md)
