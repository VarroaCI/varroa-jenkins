# frontend/AGENTS.md

## Purpose

React 19 + TypeScript + Vite dashboard for Varroa (Kubernetes-native Jenkins
operator). Talks to `varroa-bff` (`internal/bff` / `cmd/bff`) over REST +
SSE. Built with `@tanstack/react-query` for server state, `react-router-dom`
for routing, CSS Modules for styling, and OpenTelemetry web SDK for tracing.

## Ownership

Owns everything under `frontend/`: app source (`src/`), Vite/TS config,
frontend Dockerfile/nginx, and frontend-only tests. Does not own the BFF API
contract itself (`api/openapi/`, repo root) or the Go backend — only the
TypeScript consumer of it.

## Local Contracts

- **cwd gotcha**: shell cwd does not reliably persist across Bash calls in
  this harness (an rtk hook can reset it to repo root mid-session). Never
  assume you're already in `frontend/` — always anchor:
  `cd "$(git rev-parse --show-toplevel)/frontend" && npm ...`.
- **Image build context is `frontend/` and `.dockerignore` keeps the local
  tree out of it.** `Dockerfile` runs `npm ci` and then `COPY . .`. Without
  the ignore file a local `node_modules` replaces the installed packages, so
  the image ships whatever React the developer machine holds. The symptom is
  a feature that passes `npm test` and is absent live, for example a `ref`
  prop that never attaches. Keep `node_modules`, `dist`, and `coverage` in
  `frontend/.dockerignore`. The ignore file also drops `.env`: a built image
  takes its BFF address at run time from `entrypoint.sh` (`VARROA_API_HOST`),
  so a local `.env` never reaches an image.
- **Backend wiring**: set `VITE_VARROA_BFF_URL=http://localhost:8080/api/v1`
  in `frontend/.env` (see `.env.example`) to point the built app at a local
  BFF directly. Alternatively, export `VARROA_API_URL` before running
  `npm run dev` — `vite.config.ts` proxies `/api` to
  `process.env.VARROA_API_URL || "http://localhost:8080"`.
- **API client is hand-maintained, not codegen'd**: `src/api/client.ts` and
  `src/types/index.ts` are manually kept in sync with the OpenAPI contract at
  repo-root `api/openapi/` (that dir feeds the *Go* client via
  `make generate-client` / `check-client` — unrelated to this frontend).
  Per the root greenfield rule, any BFF API path/shape change must be
  reflected here in the same change, by hand — no dual-shape/back-compat
  handling.
- **Design-sync directories** (`.design-sync/`, `.ds-sync/`, `ds-bundle/`) —
  machine state/build output for an external design-sync tool (component
  previews, token/CSS bundling, AI review cache under
  `.design-sync/.cache/`). All three are gitignored
  (`# design-sync staging / build output / machine state (not committed)`).
  Do not treat them as source of truth or hand-edit generated content inside
  them; `.design-sync/conventions.md` and `.design-sync/*.json` hold the
  tool's own config if you need to touch that integration.
- Components pair `Foo.tsx` with `Foo.module.css` (CSS Modules) — see
  `src/components/`. Preserve this pairing for new components.
- **Propose card** (`pages/pipelines/ProposePipelineCard.tsx`, `proposeState.ts`): the Tenant home card that starts a `propose-pipeline` session and shows its state. Its rules are pure in `proposeState.ts`. It renders model text as text nodes only.
- **Assistant UI** (`components/assistant/`): `Markdown.tsx` is the only renderer of model and note text; never use `dangerouslySetInnerHTML`. The session key is never stored in the browser. `transcript.ts` folds frames; it drops a replayed seq. `AssistantProvider` is the one place that answers availability. `providerChoice.ts` holds the removed-provider check that the profile Model card and the admin Defaults card share. `TranscriptStream.tsx` streams a session and `TranscriptView.tsx` renders its rows; the drawer and the Tenant agent full-page transcript share them, and the caller passes the approval actions.
- **Tenant agent pages** (`pages/pipelines/TenantAgent*.tsx`, `pages/Agents.tsx`): the Tenant agent tabs, the Tenant home card, and the cluster-wide `/agents` page. Labels say "Tenant agent", never the bare word "agent". The unattended toggle asks for confirmation before it writes.
- **Tab rows**: `components/Tabs.tsx` is the only tab row. Each caller passes a unique `idBase` and an `ariaLabel`. Each caller spreads `tabPanelProps(idBase, activeTab)` on the element that holds the active panel.
- **Accent fills that carry text use `--accent-fill` + `--on-accent`, never
  `--accent` + a hardcoded label colour.** `--accent` sits in a luminance
  dead zone (honey `#C2611C`: white 4.18:1, `--text` 3.98:1 — both under
  the 4.5:1 floor; pollen is 2.63:1), so no single label colour works
  against it. `styles/tokens.css` defines a measured fill/label pair per
  theme and per `data-accent` variant; hover uses `--accent-fill-hover`.
  Decorative fills with **no** text on them (Sidebar active bar,
  `ManagedJenkins .execBar i`) correctly stay on `--accent` — non-text only
  needs 3:1, which it clears at 3.88:1. New accent-on-text surfaces must be
  measured across all four variants × both themes before landing.
- **Spec Editor curated form** (`components/specform/`): the Form tab edits
  `ingressSpec` and `miteSpec`. `podOverrides` and `resourceOverlay` stay
  YAML tiers. A non-conflict save failure shows `ApiError.serverMessage` in a
  card banner and keeps every draft. `ingressSpec.mode` and `cell.enabled` are `ui:readonly`.
- Coverage thresholds are enforced in `vite.config.ts` (`test.coverage`):
  lines 80 / branches 74.5 / functions 73.5 / statements 78.5, excluding
  `src/test/`, `src/vite-env.d.ts`, and the spec files themselves.
- **Sidebar collapse contract**: collapse is user-choice-only — localStorage
  key `varroa-sidebar-collapsed`, toggled by the foot button or `[` (guarded
  against inputs/palette); 640px is the single breakpoint and it swaps
  drawer↔bottom-tab-bar; no width/orientation ever produces the icon rail;
  door visibility unions live in `src/lib/navPermissions.ts` and the area
  sub-nav matrix in `src/lib/navAreas.ts` — single sources, never re-derive
  inline.
- **`--text-3` is not a text colour**: it fails the 4.5:1 floor on every
  shell surface (3.41 on `--surface` and worse elsewhere); use `--text-2` for
  text; `--text-3` is decorative/non-text only.
- **Status text tokens**: The five `-text` pairs (`--ok-text`, `--warn-text`,
  `--bad-text`, `--info-text`, `--idle-text`) are the ONLY status tokens
  allowed on text. Raw `--ok`/`--warn`/`--bad`/`--info`/`--idle` are
  reserved for non-text surfaces (dots, bars, borders, rings). Each theme
  (light and dark) defines its own `-text` pair — see `styles/tokens.css`.
- **Code-surface tokens**: `--code-bg` (#231910) and `--code-text` (#E8DCC8)
  are fixed (identical in both themes) and used by all YAML editors, overlay
  editors, and the log console — never hardcode the palette. `tokens.css` sets
  `color-scheme` per theme (`light` on `:root`, `dark` on `html[data-theme="dark"]`).
  Every block that paints a `--code-bg` surface also declares `color-scheme: dark`,
  so native scrollbars match the dark surface. `styleIntegrity.test.ts` enforces it.
- **Style-integrity guard** (enforced by `src/styleIntegrity.test.ts`): the
  suite fails if any `styles.X` or `styles["X"]` reference in a `.tsx` file
  has no matching class in its imported `.module.css`, or if any `var(--X)`
  reference anywhere under `src/` (`.css` or `.tsx`) doesn't resolve to a
  token declared in `styles/tokens.css` or declared locally in the same
  file (e.g. a component-scoped custom property like `--rail-w` in
  `Sidebar.module.css`). This is a whole-namespace check, not a per-prefix
  scan — a new undefined-token family can never sneak in silently. The
  suite also ratchets the two rules above: it fails if any `color:`
  declaration in `.css` under `src/` uses `--text-3`, or a raw status
  token (`--ok`/`--warn`/`--bad`/`--info`/`--idle`) instead of its
  `-text` variant. None of the four checks has an allowlist or escape
  hatch; the orphan-class allowlist is empty and new entries are not
  permitted.
- **Attention state**: `StatusPill` is the only renderer of a controller's
  needs-attention state, and `ATTENTION_LABEL` (exported from
  `src/components/StatusPill.tsx`) is the label source of truth. Fleet views
  (Dashboard, `BroodControllerPicker`, `ControllerDetail`) pass the BFF's
  `attention` field through; never re-derive it from `phase`.
- **Fleet views render reported state only**: every value on a Dashboard
  brood-health row (`data-testid="health-row"`) comes from a
  `ControllerListItem` field the BFF sent (`miteConnected`, `phase`,
  `attention`, `lastSeen`, `jenkinsHealth`, `jenkinsVersion`). Never
  synthesize, sample, or extrapolate a value the API did not report, and
  never label a view with a time window the data does not cover. Relative
  ages use `age()` from `src/components/activityTimeline.util.ts`; it is the
  single relative-time helper.
- **Dashboard connect tile** (`pages/ConnectClientTile.tsx`): shows between the metric cards and the update-center chip only when the user has no API key and the assistant is not on. Its three snippets come from `lib/mcpClientSnippets.ts`, and `ConnectClientTile.test.tsx` fails when a snippet drifts from `docs/agents/connecting.md`. The card never shows or holds a key.
- **Activity icons**: `activityTypeMeta` in `src/components/activityTimeline.util.ts`
  is the only type-to-icon source for every activity surface (timeline and
  Dashboard). Live frames pass `isActivityEvent` before they reach the feed buffer.
- **Fleet-scale rendering**: the Dashboard brood-health card renders a phase
  bar, at most eight rows, and a "View all" link. `BroodControllerPicker`
  renders its rows through `useVirtualizer` inside the scroll container
  `data-testid="controller-table-scroll"`; the sticky header stays in that
  container. Select-all reads the filtered data, never the rendered DOM.
- **Module ownership**: each page/component owns its own `.module.css` —
  no page imports another page's module. Small boilerplate (e.g. the
  `.page`/`.pageHead`/`.pageTitle`/`.pageDesc` header pattern) is
  duplicated per module rather than shared, matching the rest of
  `src/pages/`; a component like `SectionPage` that needs that pattern
  gets its own `SectionPage.module.css` rather than importing a page's.
- **Lists inside a Card fit the Admin & access content width (706 px at a 1280 px viewport).** Wrap a `.dataTable` in an element that composes `tableScroll` from `src/pages/tables.module.css`. A CSS-grid list row uses `minmax(0, …)` tracks and a fixed-width actions track, so the header and every row align. A single-value `select` keeps the text-input height; only `select[multiple]` gets a taller box.

## Work Guidance

- `npm run build` runs `tsc && vite build` — `tsc` is a strict typecheck gate
  (`strict`, `noUnusedLocals`, `noUnusedParameters` all on in
  `tsconfig.json`), not just a build step; treat type errors as build
  failures.
- Tests colocate with source as `*.test.ts(x)` (e.g.
  `src/hooks/useApi.test.ts`, `src/context/AuthContext.test.tsx`) — no
  separate top-level test tree except shared fixtures in `src/test/`
  (`factories.ts`, MSW `handlers.ts`, `render-utils.tsx`, `setup.ts`). Vitest
  runs in `jsdom` with MSW for API mocking; follow the existing
  factory/handler pattern for new API-backed tests rather than ad hoc mocks.

## Verification

```bash
cd "$(git rev-parse --show-toplevel)/frontend" && npm ci
cd "$(git rev-parse --show-toplevel)/frontend" && npm run test       # vitest run
cd "$(git rev-parse --show-toplevel)/frontend" && npm run build      # tsc typecheck && vite build
cd "$(git rev-parse --show-toplevel)/frontend" && npm run coverage   # vitest run --coverage (enforces thresholds above)
```

Equivalent repo-root targets: `make frontend-install`, `make frontend-build`,
`make frontend-test` (runs `npm run coverage`), `make frontend-docker-build`.

## Layout Map

- `src/pages/` (~109 files) — route-level screens, one per route plus
  matching `*.module.css` and `*.test.tsx` (e.g. `Controllers.tsx`,
  `ControllerDetail.tsx`, `CatalogBrowser.tsx`, `Roles.tsx`, `JenkinsRoles*`,
  `Tenants*`, `PluginsTab.tsx`, `FleetPlugins.tsx`, `Cells.tsx`,
  `CellTab.tsx`, `Locks.tsx`). Registered in `src/App.tsx` / `src/routing.ts`.
  `Cells.tsx` is the fleet Cells page at `/cells`, behind a cluster-wide
  `cellpools:read` gate (`canCellsRow` in `src/lib/navPermissions.ts`, which
  the sidebar and the More sheet both read). `Locks.tsx` is the fleet locks
  page at `/locks`, behind the cluster-wide `fleetlocks:read` gate, and it
  carries **no** sidebar row and no `navPermissions.ts` door: a caller
  reaches it by its own URL. It polls `GET /locks` on the cells cadence and
  opens no stream. That read covers every cluster, so a waiting row finds a
  holder on another cluster, and each row names its own cluster. A caller
  with the cluster-wide `fleetlocks:release` verb gets a **Force release**
  action on each `releasing` row. Its dialog requires a reason, posts
  `POST /locks/release`, shows a refusal as the HTTP status and the server
  message, and reads `GET /locks` again after every attempt. A 409 that
  carries `holderCell` adds a link to that cell's controller page, where
  Restart keeps its own permission check; the dialog makes no restart call.
  `CellTab.tsx` is the Cell tab of
  `ControllerDetail.tsx`, rendered only when the detail response reports
  `kind` `cell`. Its role, agent namespace, and placement generation
  columns all come from the cell usage read, never from `GET /tenants`: a
  member cell's Tenant lives on the member cluster only, and the record store
  holds its placement row. `ControllerDetail.tsx` keeps the active tab in the `tab`
  query parameter, not in state: a user's click pushes a history entry and a
  correction replaces one, so the Back button walks the tabs. Neither cell
  surface computes a capacity value; both render the numbers the cell usage
  read and the `CellPool` status report, and they name both caps when the
  two differ. `Cells.tsx`'s scale control sends **only the dials the caller
  edited**: `PATCH /cellpools/{name}` takes any subset, so repeating an
  unchanged value reverts a concurrent edit by another operator. A dial
  counts as edited only when the caller typed into it (a per-dial dirty
  set) **and** its value differs from the stored one — the block polls, so
  a live-spec comparison alone would turn another writer's change into a
  pending edit; an untouched input re-seeds from the live spec instead. The
  drain confirm tests the changed set, not the input value, and an Apply
  with no change sends no request. The control reports **every** refusal: a
  `409` keeps the conflict wording, and any other status renders the API
  error message, because a `400` on a dial bound is otherwise silent.
  `CatalogSources.tsx` carries a source-type select (Git or OCI) that mirrors
  the CRD's exactly-one-of rule: the form sends `repoURL` or `ociRef`, never
  both, and an OCI source has no revision. The reserved `varroa-update-center`
  row has no Edit, because the operator owns it (Sync and Delete stay).
  `TenantForm.tsx` carries an exports editor: each entry picks `job` or
  `locks` (a select, mirroring the CRD's exactly-one-of rule), the matching
  pattern input(s), and a `callers` list; client-side validation mirrors the
  CRD's CEL rules (a caller required, a non-empty job/locks, `job` never
  starting with `/`) so a bad entry is caught before submit. `Tenants.tsx`
  lists a tenant's exports as one summary line per entry, naming its kind,
  pattern(s), and callers, with a dash for a tenant that exports nothing.
- **One glob evaluator per language**: `src/lib/jobglob.ts` (`jobGlobMatches`)
  is the browser's only folder-relative job-glob evaluator, and it mirrors
  Go's `internal/rbac/jobglob`. `src/lib/jobglob.test.ts` carries the same
  eleven cases under the same names as that package's `TestMatch`, so a
  grammar change that updates one side and not the other fails a test.
  `usePermissions.ts`, `TenantScopeEditor.tsx`, and any future caller import
  it; a second browser implementation is not permitted.
- `src/pages/JenkinsLinkRedirect.tsx` — `/headless/:cluster/:namespace/:controller/*`, the door a
  Jenkins link under a headless controller opens. It calls `getControllerJenkinsLink` and replaces
  the URL with the returned Varroa page, reading the raw pathname so `%252F` survives.
- `src/pages/pipelines/` — the pipelines route tree: `PipelinesIndex.tsx`
  (`/pipelines`, lists the names of `GET /pipelines` rows only, which include
  member-cluster Tenants and controller-namespace Tenants; it does not merge the
  caller's `tenants` grant names, and a failed read shows an error with Retry;
  it redirects when exactly one name qualifies), `TenantHome.tsx` (`/pipelines/:tenant`, the jobs folder tree, a Running
  now strip, and a `Locks` card that reads `GET /pipelines/{tenant}/locks`,
  refetches on a `lock` stream frame under the same 2-second window a `run` or
  a `stage` frame uses, and polls that read on the cells cadence, because a
  lock under the tenant's own prefix that another tenant's run holds publishes
  its `lock` notification on the holder's subject, which the tenant stream does
  not carry), `PipelinePage.tsx` (`/pipelines/:tenant/:job`, one job's
  health, definition, and run list, and hosts `RunDrawer.tsx`'s parameter form; a `wait` stream frame of its own job refetches the job read, which feeds the Waiting card, and the runs table),
  `BuildPage.tsx` (`/pipelines/:tenant/:job/:run`, one run's detail; owns both
  `useEventStream` calls, the tenant run stream and the run log stream; renders
  the run's `summary` as a header row, renders its failure `digest` in a
  `Digest` card above the `Pipeline` card and below the interrupted banner, and
  renders the **Run links** card (`RunLinksCard.tsx`, `RunLinksCard.module.css`) between the
  interrupted banner and the `Digest` card. It shows the upstream run and the downstream runs,
  and its **Show more** button pages `listRunDownstream`. It renders nothing when no link exists;
  renders a pending input step in `PendingInputCard.tsx`, which has its own
  `PendingInputCard.module.css`, in a `Pending input` card between the
  `Digest` card and the `Pipeline` card; its `Tests` tab shows the report
  line, then the case table, grouped under a suite header row each time the
  suite changes, in the response order (`testCaseGroups.ts`; the Class column
  shows only when a suite holds two or more classes), then the suite table),
  `JobBreadcrumbs.tsx` draws the trail on the pipeline, build, and test case
  pages; its optional `current` prop adds the last crumb (the build or the
  test) and links the job crumb to the pipeline page. `BuildPage.tsx` keeps
  the active tab in the `tab` query parameter (the `ControllerDetail` pattern),
  and its j/k keys keep that parameter. `ShortcutLegend.tsx` owns the build
  page key list (`BUILD_PAGE_SHORTCUTS`); no other file lists those keys.
  `pageTitles.ts` is the only title formatter for the pipelines pages.
  `breakablePath.tsx` inserts break opportunities in a path. A pipelines table
  never uses `overflow-wrap: anywhere` on a cell,
  and `TestCaseHistory.tsx` (`/pipelines/:tenant/:job/tests/:case`, the recent
  outcomes of one test case). A case key is opaque: `testCaseHref` in
  `jobPath.ts` encodes it as one segment, and no page parses it. The
  history page reads the key from the raw URL path, because React Router's
  own param turns every `%2F` in a segment into `/`. A case link passes the
  class and the test name as route state, so the page names them.
  `BuildPage.tsx` keeps
  every log frame in a ref, hands `RunLogViewer.tsx` the frames plus a version
  counter, and passes the log stream's `lastEvent` down to `StageGraph.tsx` as
  a prop. The version counter changes on every frame, because one render can
  cover a whole burst of frames and `lastEvent` keeps only the last frame of
  that burst. A log frame arrives at the page through `options.onEvent`, not
  through `lastEvent`, so no frame of a burst is lost. `StageGraph.tsx` renders
  a run's stages from `GetRun`'s stage list plus live `stage` frames. It draws
  the SVG at its native size inside a horizontal scroll wrapper. A label longer
  than 22 code points is truncated and the node's `<title>` holds the full name.
  Nested-stage lists render as HTML groups under a parent-name toggle.
  `BuildPage.tsx` also renders one terminal card, in its own early return
  after the `!run` guard, for a run whose `result` is `TRIGGER_FAILED`: the
  record exists, so the `requested` and `notStarted` blocks never run for it.
  A record's `commandRefusal` renders one `role="status"` notice under the
  interrupted banner, with the command, the HTTP status, the actor, the time,
  and the error. A later successful command does not remove it.
  `RunLogViewer.tsx` renders the log stream: search, virtualized scroll, and
  ANSI colour via `ansi.ts`'s `parseAnsiLine`. `RunLogViewer` applies the
  frames the page hands it, in order, and it never calls `useEventStream`
  itself. It unmounts while another tab is active, so the page holds the
  frames, not the viewer. The stream sends every `gap` frame again on each
  reconnect, so the viewer shows one gap row per `seq`. A `gap` frame never
  changes the page's resume watermark or its `fromLine`.
  `BuildPage.tsx` passes the record's `logFinalized` flag to the viewer. The
  viewer shows its "still being written" notice only for a terminal run with
  that flag false, after the `meta` frame and before the `end` frame. The
  stream sends `end` on its first poll tick, so the flag keeps a finished log
  from showing the notice. A `#L<n>` fragment in the URL also scrolls the page
  to center the viewer; a search jump and the first-error jump do not. Follow is
  off and disabled once `runTerminal` is true and the log is finalized or the `end`
  frame arrived (`logDone` in `RunLogViewer.tsx`). The
  viewer derives each stage header from the `stageId` of the lines, not from
  frame order or `startLine`. It places a header before the first line of its
  stage, and a stage with no lines gets none. A stage selection or a
  `#stage-<id>` fragment holds as a pending jump until a header of that stage
  or of a descendant exists, the same way `pendingLine` holds. A resolved
  stage jump turns Follow off. The viewer reads the `stages` prop only for the
  descendant walk. A `#stage-<id>` fragment also scrolls the page.
  `src/components/Console.tsx` is a different, generic log-line component and
  is not the log viewer for this page — do not route pipeline log rendering
  through it. `TenantCellCard.tsx` heads `TenantHome.tsx` with the tenant's own cell.
  It owns its query on `GET /pipelines/{tenant}/cell`, the one path it reads, and
  its own `refetchInterval` of 30 seconds; the page's two other reads keep their
  current behaviour. A failed cell read, and the state `notCellHosted`, render
  `null`: the card never replaces the tenant home with a page-level recovery
  state, and it reserves no grid column. An absent `queueLength` reads
  `not reported`; no absent field renders a zero.
- **The tenant home's folder tree** — `jobTree.ts` is the pure model
  (`buildJobTree`, `defaultOpenPaths`, `visibleRows`); it imports no React, so
  every tree rule is tested with no render. `JobTree.tsx` renders one `<tr>`
  per visible node inside `TenantHome.tsx`'s own `<tbody>`, keeping the six
  frozen columns; it also owns `formatDuration`, the health glyph, and the
  sparkline, which `TenantHome.tsx` imports back for its Running now tiles. A
  folded node's descendant rows are **not rendered at all** — never hidden
  with a style, so the accessibility tree matches the page. Indentation rides
  `--depth`, declared in `JobTree.module.css` and set per row by an inline
  style; a colored `border-left` is not a depth cue. The fold set lives in the
  URL as repeated `open` parameters, written with `replace: true`; the page
  decides default-versus-explicit with `searchParams.has("open")`, never with
  the number of values. `JobBreadcrumbs.tsx` renders the pipeline page's own
  trail; `src/components/Topbar.tsx` keeps its single area crumb and is not
  merged with it. `JobTree.tsx` shows a `Pending` badge on a job row or a multibranch node when
  `pending` is true; `PipelinePage.tsx` shows a note above the cards for a pending job.
- **`src/components/TenantScopeEditor.tsx`** owns the whole `tenants`
  dimension of a `VarroaRoleBinding` scope: the entry list, the `Tenant` name
  control, the `jobs` glob list, the grammar help, the six client-side
  refusals (`validateTenantScope`), and the `matches N of M jobs you can see`
  preview, which reads `GET /pipelines/{tenant}` with the **editing caller's**
  grants and blocks nothing when that read fails. `RoleBindingForm.tsx` holds
  the whole loaded `scope` object and rebuilds it on submit by spreading that
  value, so a dimension the form does not edit (such as
  `controllerSelector.matchExpressions`) survives an edit.
- **`src/components/FederationStatus.tsx`** renders any status with a
  `federation` list (`VarroaRoleBinding` or `VarroaRole`):
  `FederationSummary` (the Role Bindings list column, the non-zero hive count
  per state, or `Not reported`) and `FederationStatusTable` (the read-only
  per-hive table on the binding edit page). Both render the reported entries
  only. `RoleBindingForm.tsx` never sends `status` on a submit. `Roles.tsx`
  renders `FederationSummary` next to a role name only when the role has at
  least one entry, because a role status holds faults only.
- **Pipeline templates** — `CatalogItemDetail.tsx` renders a
  `pipeline-template` item's `PipelineTemplateCard`: the template's
  parameters/uiSchema form (`VarroaForm`, `src/components/specform/
  VarroaFormTheme.tsx`) and a generated marker preview. It validates against
  `templateValidator` (`src/components/specform/templateValidator.ts`, an
  Ajv 2020-12 instance, separate from `ControllerSpecForm`'s draft-07
  validator, because a PipelineTemplate document declares a JSON Schema
  2020-12 subset). `templateValidatorCorpus.test.ts` reads
  `internal/bundle/pipelinetemplate/testdata/schema-corpus/` directly by
  relative path (no local mirror, unlike the plugin's Maven test, which
  cannot see the Go module tree), so this suite and the Go package's own
  `corpus_test.go` always run against the same fixture files.
- `src/components/` (~87 files) — shared UI building blocks (`Button`,
  `Card`, `Layout`, `Console`, `KVGrid`, `MetricCard`, `ConfigPipeline`,
  `OverlayEditor`, `ActivityTimeline`, route guards `ProtectedRoute` /
  `PermissionRoute`), each paired with a `.module.css`. `PermissionRoute`
  renders `LoadingSpinner` while the auth context's `permissionsLoading` is
  true, and it renders `ForbiddenPage` only after the permissions query
  answers. Every permission check answers false before that answer, so a
  denial before it is not a real answer. A failed read with no earlier
  answer renders `GenericErrorPage` with a retry (`retryPermissions`); only
  a `403` from that read renders `ForbiddenPage`.
- `src/hooks/` — data-fetching hooks wrapping React Query
  (`useControllers`, `useCatalog`, `useClusters`, `useActivityFeed`,
  `useEventStream` for SSE, `usePermissions`, `useFleetPlugins`, `useCells`).
  `useDocumentTitle` is the only writer of `document.title`; it restores the
  previous title on unmount.
  `useEventStream`'s optional `options.resumeUrl` callback is read on a
  reconnect only, never the first connect: a returned URL replaces
  `baseUrl` for that attempt, so a caller tracking a resume position (a
  byte offset, a cursor) reconnects past what it already read instead of
  replaying the stream from the start. `BuildPage.tsx` is the one caller
  that passes it, to resume the run log stream past its last-read byte
  offset. `useEventStream`'s optional `options.onEvent` callback runs for
  every parsed frame, named or unnamed, after the hook sets `lastEvent`. One
  render can cover a whole burst of frames, so `lastEvent` keeps only the last
  frame of that burst; a caller that must keep every frame reads them here.
  The hook reads both options through a ref, so a caller can pass a fresh
  closure on every render without restarting the stream.
  `useControllers.ts` carries
  the hand-maintained `ControllerDetail` TypeScript interface (not generated)
  — new backend fields such as `pluginInventory`, `PluginInventorySummary`,
  and `PluginInventoryDriftEntry` must be added here in the same change or
  the frontend silently drifts from the contract. `ControllerListItem` in
  the same file carries the cell fields (`kind`, `cellPool`, `tenantCount`,
  `maxTenants`, `warm`, `createdAt`, `unreadySince`); `kind` and `createdAt`
  are required, and the other five are absent for a controller that is not a
  cell, so a component branches on absence rather than on zero.
  `useCells.ts` holds every cell query: `useCellPools`/`useCellPool`,
  `useCellUsageList`/`useCellUsage`/`useCellUsageIndex`, and `useCellGrants`.
  `useCellUsageIndex(clusters)` is what a fleet page uses: the batch usage
  read covers one cluster, so it issues one request per cluster and returns
  a `rows` map keyed `cluster/namespace/name` (`cellUsageKey`), plus the
  `covered` and `recordStoreDisabled` cluster sets. **A cell name is unique
  inside one cluster only**, so a namespace-and-name key silently hands one
  cluster's counts to another cluster's cell; a row is keyed by the cluster
  its own request named, never by the cluster the response reports, and a
  cell whose cluster is not `covered` renders "not available" rather than a
  dash or a zero. The pool and usage
  reads poll every 15 seconds, the cadence `useControllers` uses; the Cell
  tab's own reads take an `enabled` flag and run only while that tab is
  active; the grants read holds a 60-second stale time and no interval. No
  cell surface opens an `EventSource`.
- `src/context/` — `AuthContext`, `ThemeContext`, `ComposerContext` (bundle
  composer editing state) as React context providers.
- `src/lib/` — pure helpers: `overlay.ts` (resource-overlay merge preview),
  `pluginDiff.ts`, `versionCatalog.ts`, `compat.ts` (update-center catalog
  grouping + advisory verdict presentation: `groupByPlugin` collapses a plugin's
  stored versions into one row, `worstVerdict`/`isWarningVerdict` mirror the
  operator's precedence. A compat badge is **advisory** and must never be wired
  to a disabled state — derivability blocks, compatibility only advises, and
  `CatalogBrowserUpdateCenter.test.tsx` asserts it), `broodTargets.ts`
  (`broodTargetShape`: maps `cluster/ns/name` picker keys to brood-API tenancy
  shape; shared by the brood-operation modal and the brood-schedule form),
  `uuidv7.ts` (`uuidv7`: mints a UUID version 7 client token, layout matching
  the server's own reader in `internal/api/handlers_pipelines_write.go`;
  `RunDrawer.tsx` mints one token per drawer open, so a repeated submit
  carries the same token and the BFF answers with one run), `fleetLocks.ts`
  (`holderOf`/`holdsLock`: the single holder-grouping rule for both lock
  tables — a waiting row's holder is the row of the same lock name in the
  fetched list whose state is `held` or `releasing`, and no read answers a
  holder or a queue position).
- `src/api/` — `client.ts`, the single hand-written fetch client typed
  against `src/types/index.ts` (mirrors CRD/API shapes: `Controller`,
  `ComposedBundle`, `VarroaRole`, `JenkinsRole`, `BroodRun`, etc.).
- `src/types/` — `index.ts` (API/CRD-shaped types) and `auth.ts`.
- `src/routing.ts` — route-path helpers (`controllerRoute`, `withCluster`,
  `clusterQuery`) used to build cluster-scoped links consistently across
  pages.
