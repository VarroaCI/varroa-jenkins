# cmd/varroactl/

## Purpose

`varroactl` is the Varroa CLI: a `kubectl`-style client over the BFF REST API
(`cmd/bff`), plus an MCP bridge that lets any MCP-capable agent (Claude Code,
etc.) drive Varroa through the same API. ~62 files, one per noun/verb/concern.

## Ownership

Owns command parsing, output rendering, local config/context storage, and the
three auth flows. Does not own API business logic — every command is a thin
wrapper around `pkg/client` (generated OpenAPI client) calls against the BFF;
new capabilities are added to the BFF/OpenAPI spec first, then surfaced here.

## Local Contracts

- **`export catalog`** (`export_catalog.go`) — fetches the source tree with
  `bundle.FetchCatalog`, reads the index with `bundle.ParseCatalogIndex`, and
  pushes the tree as one catalog layer. It never calls `Resolver.Materialize`.
  The manifest reference is the `--to` target. An untagged `oci://` target
  takes `:<name>` as its tag. A `spec.secretRef` source needs `--secret-file`.
  The command reads the Secret from that file, never through the BFF, and
  applies `bundle.CheckGitSecretHost` with the controller's error text.
- **Entrypoint** — `main.go` just calls `run(os.Args[1:])` (`root.go`).
  `root.go` maps errors to exit codes: `usageError` / cobra parse errors → 2,
  other errors → 1, success → 0 (`isUsageError`, `run`).
- **Latest pins** — `export plugins` resolves every `latest` entry through
  `ResolveLatestForCore` before download, and a pack never carries `latest`.
- **Command tree (root.go)** — two registration seams so noun files don't
  touch shared root/verb-parent files:
  - `registerNoun(nounRegistrar)` — attaches a noun (e.g. `controllers`,
    `groups`, `tenants`, `catalog`) onto the shared verb parents
    (`get`/`describe`/`create`/`delete`/`edit`/`patch`) built by
    `newRootCmd()`.
  - `registerRootCommand(rootRegistrar)` — adds a top-level, non-CRUD command
    directly onto root (`login`, `logout`, `whoami`, `mcp`, `attach`,
    `watch`, `passwd`, …).
  - `attach.go` has two lookups: `findCommand` EXITS the process when a segment
    is missing (use it when the target must already exist) and `lookupCommand`
    returns nil (use it when this registrar may legitimately have to create a
    brand-new verb parent, as `upload_plugin.go` does for `upload`).
  - `crdnoun.go` provides a generic `crdNounOpts` helper so most CRD-backed
    nouns (controllers, groups, catalog, composedbundles, rbac, runners, …)
    implement get/describe/create/delete/edit/patch by filling in a struct
    rather than hand-writing each verb. `crdNounOpts` also carries five
    optional typed callbacks: `List`, `Get`, `Create`, `Patch`, and `Delete`.
    Each callback takes the generated `*client.ClientWithResponses`. A noun
    that supplies a callback uses the generated client for that verb. A noun
    that supplies none keeps the hand-rolled `rawRequest`. `Patch` sends a
    JSON merge patch (`jsonpatch.CreateMergePatch`). A noun that supplies
    `Patch` also gains a `patch` subcommand (the same `-p/--patch` flag
    `controllers.go` hand-rolls) and routes `edit` through that merge patch
    instead of `editLoop`'s default full-replace `PUT`. Only `runners`
    (`runners.go`) supplies all five callbacks. Every other noun through
    `crdnoun.go` is unchanged.
  - One file per noun, each paired with a `_test.go`: `controllers.go` (+
    `controllers_actions.go`, `controllers_edit.go`), `groups.go`,
    `tenants.go`, `catalog.go`, `composedbundles.go`, `rbac.go`, `apikeys.go`,
    `clusters.go`, `broodop.go`, `broodschedule.go`, `provisioning.go`,
    `users.go`, `runners.go`, `crdnoun.go`/`editcrd.go`/`describe.go` (shared
    verb machinery).
  - **`runners.go`** — the `runners` CRD noun (alias `rnr` only; the varroa-cli
    delta forbids the alias `runner`, since `runner_register.go` reserves that
    word for the top-level ergonomic group). Cluster-scoped with no cluster
    segment (`/api/v1/runners`, like Tenants). All five generated-client
    callbacks convert between the generated client's flat `{name, spec,
    status}` DTO and the `{metadata: {name}, spec, status}` document shape
    the shared machinery (`Columns`, `printDescribe`, `editLoop`) expects.
  - **`cellpools.go`** — the `cellpool` noun (alias `cellpools`) serving
    `get` and `describe` only, plus the top-level verb `scale`, registered
    from this same file through `registerRootCommand` so no framework file
    changes. Cluster-scoped: every command refuses `-n`/`-A` with a usage
    error before any request. `get`, `describe`, and `scale` take `--cluster`: a
    list with no flag covers every cluster and prints a `CLUSTER` column (an
    unanswered cluster is a warning on stderr); a single pool with no flag is
    read from the BFF's own cluster. `scale cellpool <name> --warm N [--max-cells N]`
    sends only the flags the caller actually passed (`Flags().Changed`), so an
    unnamed dial keeps its stored value; no flag at all, and a negative value,
    are usage errors raised before the request. There is deliberately no
    `create` and no `delete`: the API serves no such route.
  - **`runner_register.go`** — the top-level `runner` command group:
    `register` (design D12 — validates the name locally as a DNS-1123 label
    before any request, creates the `Runner`, polls up to 60s for
    `status.token`/`status.nodeName`, then prints the launch instruction —
    the exact `java -jar agent.jar …` command for `inbound`, the SSH Secret
    expectation for `ssh`), `list` (`--tenant` filter), and `delete`. `list`
    and `delete` reuse `runners.go`'s own verb closures directly, so they
    delegate to the identical generated-client calls as `get runners` and
    `delete runners`.
- **`tenants.go`** — a generic `map[string]any` JSON pass-through: get,
  describe, create, delete, and edit all treat `pipelines` (including its
  `exports` list) as one opaque nested object, so a new field under
  `spec.pipelines` needs no per-field mapping here to flow through every
  verb.
- **BFF client** — `client.go`: `apiClient(cmd)` resolves the active context
  (`resolveContext` in `config.go`) and builds a `*client.ClientWithResponses`
  from `pkg/client` (generated by `make generate-client` from the OpenAPI
  spec) via `client.New(baseURL, apiKey, client.WithUserAgent("varroactl/"+
  version))`. `errFromResponse`/`apiErrorf` normalize BFF error envelopes
  (`{"error": "..."}`) into Go errors.
- **Config/contexts** — `config.go`: file at `$VARROACTL_CONFIG`, else XDG
  (`$XDG_CONFIG_HOME/varroactl/config.yaml` or
  `os.UserConfigDir()/varroactl/config.yaml`), YAML via `sigs.k8s.io/yaml`.
  Holds named contexts (server URL + API key + optional
  defaultNamespace/defaultCluster); `resolveContext` layers explicit flags
  over the active/named context.
- **Login (`login.go`)** — `varroactl login` dispatches to exactly one of
  three mutually-exclusive flows, all converging on `validateAndStore`:
  - `--api-key` (or `-` for stdin) → `loginWithAPIKey`.
  - `--username` → `loginWithUsername` (local/ldap password prompt via
    `golang.org/x/term`, `--password-stdin` supported). It refuses an OIDC
    server unless `/auth-config` reports `localAccounts: true`.
  - default → `loginBrowser`: opens a loopback listener on `127.0.0.1:0`,
    generates a random state nonce (`crypto/rand` + `base64.RawURLEncoding`),
    opens `<server>/cli-auth?port=<port>&state=<state>&name=<hostname>` in
    the browser (`github.com/cli/browser`, overridable via `openBrowser` for
    tests), and waits (default 5 min, `--timeout`) on a one-shot
    `http.Server` callback handler that verifies the state with
    `crypto/subtle` constant-time compare before storing the token.
- **MCP bridge (`mcp.go`)** — `varroactl mcp` (default, no subcommand):
    stdio↔HTTP bridge — reads NDJSON JSON-RPC from stdin, POSTs each message
    to `<server>/api/v1/mcp` with `Authorization: Bearer <token>` from the
    resolved context, and writes responses back to stdout (JSON or
    SSE-parsed via `pkg/client/stream.ParseSSE`), all diagnostics to stderr.
    `varroactl mcp serve --listen ADDR` instead starts a local
    `httputil.ReverseProxy` in front of the same `/api/v1/mcp` endpoint
    (injecting the bearer token, stripping client auth) for MCP clients that
    speak HTTP directly; `--listen` is hard-restricted to a loopback address
    (`isLoopback`).
- **Output rendering (`output.go`)** — shared `printTable`/`printJSON`/
  `printYAML`/`printName` for the `-o {table,wide,json,yaml,name}` contract
  used across every noun's `get`/`describe`.
- **Streaming/watch** — `watch.go` (`varroactl controller watch`, polling
  render loop) and `streamcmds.go` (SSE-backed live commands) both build on
  `pkg/client/stream`.
- **Pipelines (`pipelines.go`)** — `run <tenant>/<job>` triggers a build (`--param
  key=value`, repeatable; `--follow`/`-f` waits for the run to appear, then
  streams its log until it ends and exits non-zero on a non-success result).
  `builds <tenant>/<job>` lists runs (`-o {table,wide,json,yaml,name}`, table
  columns `BUILD RESULT BRANCH DURATION AGE`). `logs` gains a run form
  instead of a second command: a UUIDv7 argument names a run and everything
  else names a controller, one-shot by default, `-f`/`--follow` streams
  either form until it ends. The run-log follow (`followRunLog`) keeps a
  `runLogWatermark` from `line` events only, never from `gap` frames. It
  reconnects through `stream.Config.ResumeURL` with
  `from=<off+1>&fromLine=<n+1>`, and it skips a line at or below the
  watermark. `run`/`builds` register as new top-level
  commands (`registerRootCommand`); the run form extends the existing
  `logs` command in place via `findCommand(root, "logs")`.
- **`lint jenkinsfile` (`lint.go`)** — `varroactl lint jenkinsfile --tenant <tenant> [--file Jenkinsfile]` calls `POST /pipelines/{tenant}/lint` through the generated `LintJenkinsfileWithResponse`. Exits non-zero on any error or refusal. It has no `--allow-unavailable` flag.
- **`lint marker` (`lint.go`)** — `varroactl lint marker --tenant <tenant>
  [--marker .varroa/pipeline.yaml]` posts the marker file to `POST
  /pipelines/{tenant}/markers/validate` directly (that path has no generated
  `pkg/client` method yet, so this file builds the request itself, the way
  `activity.go` does for a path the generated client does not cover). Exits
  zero on an empty findings list, non-zero otherwise, printing each finding's
  severity/path/message. Contacts no cell and reads no record-store data.
- **`locks list` (`locks.go`)** — `varroactl locks list [--tenant TENANT]`
  reads every fleet lock through the generated client's
  `ListAllFleetLocksWithResponse` (`GET /locks`, cluster-wide
  `fleetlocks:read`), or one tenant's locks through
  `ListTenantFleetLocksWithResponse` (`GET /pipelines/{tenant}/locks`). Columns:
  `NAME STATE RUN TENANT CLUSTER JOB AGE`; `wide` adds `SINCE` and `LINK`.
  AGE counts from `waitingSince` on a waiting row and from `acquiredAt`
  otherwise. `-o name` prints the lock name of each row, so a name repeats
  for a holder and its waiters; a script that needs the run uses `-o json`.
- **`locks release` (`locks.go`)** — `varroactl locks release NAME --run
  RUN_ID --reason TEXT` force-releases one releasing fleet lock through the
  generated client's `ReleaseFleetLockWithResponse` (`POST /locks/release`).
  A missing name, run, or reason is a usage error raised before any
  request. A 409 whose `JSON409.HolderCell` is set appends the
  `varroactl restart controller NS/NAME --cluster C` command for that cell.
- **`break-glass` (`breakglass.go`)** — `varroactl break-glass NAME [-n NS]
  [--cluster C] --reason TEXT [--ttl 15m] [--target URL] [--listen ADDR]`
  requests a short-lived admin grant for a headless controller through the
  generated client's `BreakGlassControllerWithResponse`, then serves a
  loopback `httputil.ReverseProxy` that injects the granted token.
  `--listen` is loopback-only (`refuseNonLoopbackListen`, reusing
  `isLoopback` from `mcp.go`), refused with exit code 2 before any network
  call. The Director is a struct literal, not a wrapped
  `httputil.NewSingleHostReverseProxy`, because reading its `Director`
  field is deprecated. `newBreakGlassProxy` rewrites `Location`,
  `Content-Location`, and `Refresh` response headers that carry the
  grant's location URL, replacing that prefix with the proxy's
  own base URL. The command prints the matching `kubectl port-forward`
  command, the bound URL, and the expiry, then exits 0 when the token
  expires or the command is interrupted.

- **Export bundle (`export_bundle.go`)** — the layer root is the resolved `--path`
  directory only. The artifact ref is the explicit `oci://` tag when the destination
  has one. Otherwise it is `<repo>-<path>`, with credentials and the scheme removed
  and `/` and `:` replaced by `-`. The manifest carries
  `org.opencontainers.image.source` (credentials removed),
  `org.opencontainers.image.revision` (the commit SHA, omitted when empty), and
  `dev.varroa.bundle.path`. `pushBundleAsOCIArtifact` takes the annotations;
  `export catalog` passes nil.
- **Snapshot destinations** — `export bundle` and `export catalog` refuse `uc://` with a usage error before any clone, BFF call, or store write (`rejectUCSnapshotDest`). `export plugins` keeps the runtime `errUCNotSupported`.
- **Release bundle (`export_release.go`, `import_release.go`)** — `export release`
  writes one air-gap transfer artifact: the chart, the images that the rendered
  chart and the selected profiles name, one plugin pack per profile, and
  `release-manifest.json` with a digest for every blob. `import release` verifies
  the manifest digest that the operator supplies, then every blob, then every
  artifact tag, and only then pushes. A failed check pushes nothing. The
  `--manifest-digest` flag is required, with no skip mode.
  - The `helm` binary is an external dependency. The export runs `helm template`,
    `helm show chart`, `helm show values`, and `helm package`, through `--helm`.
  - The export never writes values content to the bundle. The manifest records a
    file name and a digest for each values file.
  - An image needs an exact tag or a digest. The tag `latest` fails the export.
    An image must match a row of the `imageValueKeys` table (`release_images.go`),
    a selected profile's core image, a `--bundle-dir` pod template, or an
    `--image` flag, or the export fails with "has no known source". Add a row to
    the table for each new chart value that holds an image, and keep
    `TestImageValueKeys_MatchChartValues` green.
  - A bundle directory supplies JCasC pod template images. The agent variable
    `${varroa_jenkins_agent_image}` resolves to `jenkins.agentImage`. Any other
    variable fails the export.
  - The profile pack layout tag is the profile name, so an update center import keeps
    that reference name. The `--uc` path reuses `importPackToUC`, which
    `import --to uc://` also uses.
  - Each `unseen` source names an image family that the export cannot read. Keep
    the list in `releaseUnseen` and in the air-gapped guide consistent.
  - `export plugins` and `export release` share `preparePack`, which downloads and
    checks plugins and writes nothing. Only `pack.write` touches a store.

## Work Guidance

- New API surface goes in the OpenAPI spec + `pkg/client` (regenerated via
  `make generate-client`) before it goes in a `varroactl` command — the CLI
  must not hand-roll HTTP calls around the generated client.
- New CRD-backed nouns should try `crdnoun.go`'s generic verb machinery
  before hand-writing get/describe/create/delete/edit/patch.

## Verification

```bash
make build-cli                          # compiles bin/varroactl
go test -race -count=1 ./cmd/varroactl/...
```

`TestLogin_BrowserDenied` is a known CI flake (browser-loopback timing),
not a real regression signal by itself.
