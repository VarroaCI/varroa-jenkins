# internal

## Purpose

Home of the Go backend implementation: the `varroa-operator` reconciler, the
`varroa-gateway` mite gRPC server, and the `varroa-bff` HTTP/SSE API server
share this tree. Each subpackage is either a durable domain (gets its own
`AGENTS.md`, listed below) or a focused, single-responsibility helper package
(documented inline here).

## Ownership

- Owns: repo-wide conventions for how backend packages are organized and
  cross-referenced. Does not restate root-level architecture (see
  `../AGENTS.md` Architecture Map) or CRD schemas (`api/v1alpha1/`).
- Each child package below owns its own internals; this doc is the index and
  the contract for the ones too small to warrant their own file.

## Local Contracts

One-line contract per package without its own `AGENTS.md` (verified by
reading each package's entry point):

- **`crdstore/`** — the generic typed store for `varroa.dev` CRDs: an
  8-method unstructured-level `Backend` seam (implemented by
  `controller.ClientsetClient` and the in-memory `Fake`), package-level
  generic helpers (`Get[T]`/`List[T]`/`Apply[T]`/`ApplyOwned[T]`/`Create`/
  `Update`/`Delete`/`PatchStatus`/`PatchAnnotations`/`PatchFinalizers`), and a static
  type→GVR registry covering all 17 kinds. Every CRD read/write in the
  operator, BFF, and MCP goes through it; new kinds register here instead
  of adding interface methods. Tests seed `crdstore.Fake` (upsert
  semantics; re-seed after mutating a seeded object) and assert on its
  `StatusPatches`/`MetaPatches` recorders. `ApplyOwned` is the guarded sibling
  of `Apply`: `Apply` is a full-object replace whose internal `Get` never
  consults ownership, so a pre-call check cannot protect anything. `ApplyOwned`
  Gets, creates when absent (looping back to `Get` on `AlreadyExists`), returns
  `ErrNotOwned` for a live object the caller's predicate rejects, and Updates
  carrying the **live** resourceVersion so a concurrent write conflicts instead
  of being clobbered.
- **`auth/`** — OIDC/Dex, local, and LDAP auth providers (`AuthMode` =
  `oidc|local|ldap`) plus HTTP middleware (`middleware.go`) that resolves
  request claims; subpackages `auth/local`, `auth/ldap`, `auth/identity`,
  `auth/userdirectory`, `auth/schedule` hold provider-specific logic.
- **`apikey/`** — `vk_`-prefixed API key generation/parsing (`token.go`:
  `Generate`/`Parse`, prefix+secret split), verification HTTP handler
  (`httphandler.go`), and in-memory last-used tracking with a periodic flush
  (`lastused.go`).
- **`jenkins/`** — HTTP client for the Jenkins REST API, used by mite
  sidecars to manage their local Jenkins instance (crumb handling, retries).
- **`overlay/`** — strategic-merge-patch engine over StatefulSet/Service/
  Ingress for `Controller.spec.podOverrides`/`resourceOverlay`; shared by the
  operator's apply path and the BFF's live merge-preview endpoint.
- **`transport/`** — the interface through which the operator sends
  commands to and reads state from mites, decoupled from gRPC/NATS specifics:
  `LocalRegistry` (in-process, gateway-colocated) and a NATS JetStream-backed
  bus transport (`bus_transport.go`, `bus_read_model.go`,
  `bus_read_model_transport.go`) for the multi-cluster case where operator
  and gateway run in different clusters. `StreamDegraded` reports whether the
  gateway's bus→stream bridge is broken (read from `bus.Presence`; always false
  for `LocalRegistry`, which has no bus hop) — a mite can hold a healthy gRPC
  stream while receiving no desired state at all. Not related to git — bundle
  git materialization lives in `bundle/resolver.go`.
- **`mcp/`** — MCP (Model Context Protocol) Streamable-HTTP server exposing
  Varroa's CRUD surface as MCP tools; built on `internal/api.Dependencies` so it
  stays in lockstep with the REST API surface. `handler.go` wires the server,
  `proxy.go` forwards to per-controller Jenkins MCP servers, and tool
  registration is split one file per domain (`tools_controllers.go`,
  `tools_users.go`, …) with `tools.go` holding `registerAllTools`, `resultJSON`,
  and the shared argument helpers. Six invariants the compiler will not enforce:
  (1) return every tool result through `resultJSON`, never
  `mcp.NewToolResultJSON` — MCP requires `structuredContent` to be a JSON
  object, and `resultJSON` boxes collections as `{"items": …, "count": n}` so
  array-returning tools stay legal; (2) tools that reach the operator through
  `deps.Brood` must pass `Namespace` explicitly in `ControllersCreateArgs` — the
  operator sets `cr.Namespace` from the request field, not from the marshalled
  CR, so omitting it 404s; (3) credential stripping happens inside `resultJSON`
  via `api.SanitizeObject`, shared with the REST layer — it is deliberately at
  that one chokepoint rather than per tool, because a per-call-site omission
  is exactly the failure mode a 64-tool surface makes inevitable, so a tool
  that bypasses `resultJSON` bypasses redaction; (4) register tools through `addTool`
  with a `toolKind`, never `mcpServer.AddTool` — mcp-go's `NewTool` defaults
  every annotation to its most alarming value (`destructiveHint` and
  `openWorldHint` true), so an unclassified tool ships those. `openWorldHint`
  must be set explicitly even when false. `expectedToolKinds` in
  `annotations_test.go` is exhaustive and fails on any unlisted tool;
  (5) guard every optional `api.Dependencies` field before an *unconditional*
  dereference, returning a tool error the handler cannot work without it
  (`if deps.Authorizer == nil { return mcp.NewToolResultError(…) }`) — the BFF
  wires them conditionally, and an unguarded dereference panics the request
  instead of returning a usable error. A dep a tool uses only
  conditionally needs no such guard: `get_controller_logs` deliberately skips
  authorization when `Authorizer` is nil while still guarding `Client`, which it
  cannot read logs without; (6) declared arguments and
  applied fields must match in both directions. Input schemas *are* validated, so
  a field the handler reads but the schema omits is unreachable, and an argument
  the schema declares but the handler ignores is accepted and silently dropped.
  The reverse also bites: a spec field the REST layer patches freely but no
  tool argument names is unreachable from MCP (e.g. `spec.hibernation`).
  `create_controller`/
  `update_controller` answer that with named shorthands over a
  `controllerSpecPatch` merge map plus a generic `spec` object passthrough.
  Unrecognized arguments are **rejected by name** (`unknownControllerArgsError`),
  not dropped: "nothing to update" without naming them reads as a malformed
  request rather than an unsupported field, and dropping one that arrives
  alongside a valid field would succeed while discarding half the call. The
  named shorthands never zero a field (an empty `version` means "not supplied");
  `spec` is a literal merge patch, so an explicit `""`/`null` there clears,
  matching the REST body. Two update tools instead decide on **key presence**
  (omit = preserve, present = set/replace/clear): `update_composed_bundle` and
  `update_catalog_source`. Every other update tool still reads the value, so
  `""` there means "not supplied" — do not generalize either rule across the
  package. `update_composed_bundle` replaces `inputs` wholesale (the list is the
  merge order, so a partial edit has no meaning) and rejects an empty one;
  `update_catalog_source` clears the abandoned source kind's fields when the
  other kind is set, mirroring the CRD's at-most-one rule via
  `validateCatalogSourceSpec`. The catalog-source and composed-bundle tools read
  non-string scalars only through `boolArg`/`intArg` (`tools.go`): both report
  presence separately, and `intArg` rejects a fractional or out-of-range JSON
  number rather than truncating it. Older tools still cast `float64` inline;
  move them to `intArg` when touched; (7) activity reads go through
  `deps.Backfill` (the source the REST `/activity` handler uses), never
  `deps.ActivityStore` — the ring is only fed in retention-off mode, so a tool
  reading it directly returns an eternally empty feed on any stream-mode brood;
  and brood-path mutation errors must be built with `broodFailureToolError` so
  the operator's failing preflight checks survive into the tool error instead
  of collapsing to a bare "preflight failed".
  Listing tools return a projection (`project.go`), not full CRs; a declared
  `outputSchema` covering more than one result shape must use `anyOf`, since
  `oneOf` requires exactly one match and the shapes overlap. Output schemas are
  **not** server-validated — `handler.go` enables input validation only — so
  they are asserted in tests.
- **`preflight/`** — provisioning preflight `Check`s (pass/warn/fail) run
  before/alongside `handleProvisioning`, consuming `internal/tenancy` for
  namespace-state checks.
- **`observability/`** — parses/validates the
  `observability.varroa.dev/{providers,capabilities}` intent annotations on
  `CatalogItem`/`ComposedBundle`; downstream consumers push the resolved
  intent to mites for remote-cluster metrics.
- **`tenancy/`** — namespace classification and lifecycle (`NamespaceState`)
  for the control plane; the seam multi-cluster/team-scoping features build on.
- **`telemetry/`** — OpenTelemetry config helpers, incl.
  `Disabled()` reading `VARROA_TELEMETRY_DISABLED`.
- **`signing/`** — shared RSA JWT signing + JWKS primitives; both the mite
  operator-JWT signer (`internal/mite`) and `auth/local` consume the same
  `Signer` so the JWK `kid` matches across every token a given keypair issues.
- **`oci/`** — oras-go v2 wrapper: `BlobStore` interface (`store.go`) with
  `LayoutStore` (OCI image-layout dir) and `RegistryStore` (remote registry,
  docker-config-path auth via `CredentialConfigPath`) implementations, plugin-pack
  build/read (`pluginpack.go`), and cross-store `Copy`. Owned by the update-center
  epic's C1; consumers (`internal/updatecenter`, `internal/bundle`, `cmd/varroactl`)
  use the interface, never redefine it. Media types/annotations are a pinned
  contract shared with `charts/varroa`'s update-center values.
  `PackConfig.Kind` (`profile`|`addon`) is the sole pack discriminator — an
  empty `Profile` is not an implicit one — and is required on write and on
  read. `internal/oci` owns the **only** encoder for the structured layer
  annotations (`tags`, `dependencies`); consumers must go through
  `ResolvedPlugin`'s typed fields rather than hand-rolling the JSON.
  `ApplyHPIMetadata` is the single implementation of the derived-annotation
  contract every pack producer shares. The encoder OMITS empty values, so an
  absent annotation means "empty", not "unknown"; the discriminator for a pack
  that predates the annotation contract is `PackConfig.Kind` being unset.
  `PackConfig.UploadedBy`/`UploadedAt` are provenance for an authenticated user
  upload and are empty for every other producer.
- **`updatecenter/`** — the in-cluster Jenkins update center service behind
  `cmd/updatecenter`: metadata generation (`update-center.actual.json` +
  JSONP wrapper), blob download with sha256-verified pull-through from
  `updates.jenkins.io` (upstream sha256s are **base64** in update-center.json —
  not hex), Bearer-token OCI-layout-tarball import, and the `/api/v1/inventory`
  JSON consumed by the operator's gap reconciler and the BFF proxy.
  `POST /api/v1/plugins` (`upload.go` + `closure.go`) is the **push** supply
  tier: PARSE → declared-set precondition → D4 admission → PLAN → VALIDATE →
  COMMIT, writing nothing until every mandatory dependency resolves. It requires
  `VARROA_UC_SINGLE_WRITER` — an upload is a read-modify-write against a store
  with no conditional-push primitive, so a second writer cannot be excluded by
  any in-process lock. `/api/v1/inventory` additively carries the five
  `dev.varroa.plugin.*` metadata fields, dedupes on the **whole canonical
  entry** rather than `(name, version)` — `ListManifests` is unordered on both
  backends, so first-wins was never deterministic — and **degrades gracefully
  on an incomplete store scan**: it serves every readable pack and discloses
  each unreadable one by ref+error in the response's `skippedPacks` array
  (empty/`omitempty` when nothing was skipped), only failing closed with 503
  when the scan found **no readable pack at all**. `listPackInfos` returns
  those per-ref failures (`[]skippedPackInfo`), not just a count; the metadata
  and download routes discard the second value and keep serving partial
  results as before. A consumer that **prunes** against `/api/v1/inventory`
  (the operator's derived-catalog sync) MUST treat a non-empty `skippedPacks`
  as a reason to withhold pruning for that pass — a partial listing is a lower
  bound on store contents, not proof the missing plugins are gone; a
  non-pruning consumer (BFF display, gap analysis) may serve/log the partial
  result directly. It writes one **addon pack per plugin** (the uploaded
  plugin last, at `upload-<hex12>`), because an addon pack is exactly one plugin
  with an empty profile. `ucmeta` additionally exposes outcome-typed
  `ResolveExact`/`ResolveSatisfying`: a provable negative (`NotListed`, 422) and
  an unprovable one (`SourcesDegraded`, retryable 503) must not be conflated,
  and a healthy source's answer always beats another source's degradation.
  The declared plugin set arrives as a mounted file
  (`VARROA_UC_DECLARED_PLUGINS_FILE`, re-read per request) because the service
  has no Kubernetes client; an UNREADABLE file rejects an upload rather than
  being treated as "nothing is declared". Served-metadata version precedence is
  total: declared-version eligibility, then highest by `pluginver`, then lowest
  layer sha256 — never `ListManifests` order, which is backend-dependent.
- **`jenkinsver/`** — parses/compares Jenkins version strings from container
  tags (e.g. `2.479.3-jdk17`); backs `JenkinsVersionProfile` resolution
  (exact → LTS-line → embedded baseline). **Core tags only.** It cuts at the
  first `-` and requires numeric segments, so a plugin version such as
  `4.5.14-269.vfa_2321039a_83` silently truncates. Do NOT extend it to plugin
  versions — that is `pluginver`'s job.
- **`plugininv/`** — pure controller plugin inventory collection, canonical
  hashing, and closure-aware provenance classification. No imports from
  `internal/mite`, `internal/controller`, or `internal/api`. Consumers:
  `cmd/mite` (collector), `internal/controller` (classifier).
- **`pluginver/`** — orders Jenkins **plugin** versions: a structure-for-structure
  port of `hudson.util.VersionNumber` (Maven `ComparableVersion` with Jenkins'
  qualifier table, where `snapshot` sits below `alpha`). It is what makes a
  `Plugin-Dependencies` entry a **minimum** rather than a pin. The API is
  **total** — `Compare(a, b string) int` and `AtLeast(have, want string) bool`,
  every string parses, no error and no `ok`. Do not add a parse-failure return.
  Deliberately separate from `jenkinsver`: merging them would truncate plugin
  versions at the first `-` and cannot express `1-1 < 1.1`. Upstream's ordering
  is antisymmetric but NOT transitive across degenerate qualifier forms; the
  port reproduces that rather than "correcting" it, because a corrected ordering
  would disagree with what Jenkins resolves. Consumers: `internal/updatecenter`
  (closure planner, served-metadata precedence), `ucmeta`.
- **`pluginrange/`** — parses and matches version-range expressions against
  installed plugin versions (e.g. `<=4.0.0`, `>=4.0.0,<4.1.0`). Consumes
  `internal/pluginver` only; never extends `internal/jenkinsver`. The grammar
  is comma-separated AND clauses with longest-prefix-first operators. There is
  no `unknown` match verdict — `pluginver` is total. Allocated to T2.2 by R24;
  T3.2's approved-plugin set must consume this package, not fork it.
  Consumers: `internal/api` (fleet plugin queries).
- **`hpi/`** — parses `META-INF/MANIFEST.MF` out of a Jenkins `.hpi`
  (`Short-Name`, `Plugin-Version`, `Long-Name`, `Jenkins-Version`,
  `Plugin-Dependencies`). Implements the JAR line format: continuations are
  unfolded **before** any key split, because writers wrap at 72 bytes and a
  real `Plugin-Dependencies` value is always folded. Contains **no version
  comparison at all** — `Dependency.Min` is a minimum recorded verbatim as an
  opaque string, never normalized or decomposed — comparison is `pluginver`'s.
  Consumers: `internal/oci` (layer annotations), `cmd/bootstrapdeps` (closure
  walk), `internal/updatecenter` (upload PARSE).
- **`pluginresolve/`** — pure plugin-dependency-closure library, no
  filesystem/network I/O of its own: `Resolve` walks a worklist of
  `(name, minVersion)` requirements against a caller-supplied `MetadataSource`
  (`UpstreamSource` wraps `ucmeta.Resolver`; `InClusterSource` walks an
  `oci.BlobStore`'s plugin packs with a version/completeness/manifest-digest
  tie-break), raising minimums with `pluginver.AtLeast` — never `jenkinsver`,
  which is reserved for the separate `checkCoreFloor`/`AssertCoreFloor` gate
  against a plugin's/the root HPI's `RequiredCore` — capped at
  `maxResolveDepth = 32`. Also holds the bootstrap-closure logic moved from
  `cmd/bootstrapdeps` (`ResolveClosure`, `AssertBootstrapClosure`,
  `HTTPFetcher`), presence-only with no version comparison.
- **`ca/`** — internal certificate authority (ed25519 + x509) issuing mTLS
  certs for mite↔gateway `CommandStream` connections; bootstrap HMAC token
  verification also lives here.
- **`profileview/`** — small DTO helpers for `JenkinsVersionProfile` views,
  e.g. `PluginLinesFromYAML` extracting pinned `artifactId[@version]` lines
  from a materialized `plugins.yaml` for display.
- **`logging/`** — thin `log/slog` wrapper (`logging.New`) standardizing
  level/format (`json`/`text`) across all three binaries.
- **`hibernation/`** — shared controller-hibernation contracts: webhook replay
  constants, the BFF/operator wake interstitial renderer, and the operator's
  informer-backed `wakeserver` HTTP handler.

## Verification

```bash
make test                                          # all Go tests, race detector
make lint                                          # golangci-lint v2
go test -race -count=1 ./internal/<pkg>/...        # single package
```

## Child DOX Index

- [controller](controller/AGENTS.md) — controller reconciler: CR lifecycle, provisioning, StatefulSet/RBAC/ingress, version resolution
- [api](api/AGENTS.md) — BFF HTTP/SSE handlers and REST API surface
- [mite](mite/AGENTS.md) — gateway gRPC server bridging mite sidecars to NATS/operator
- [bundle](bundle/AGENTS.md) — ComposedBundle composition, git/catalog resolution, JCasC merge
- [rbac](rbac/AGENTS.md) — VarroaRole/JenkinsRole resolution and role-strategy YAML generation
- [bus](bus/AGENTS.md) — NATS/JetStream transport: subjects, streams, KV buckets, cluster directory
