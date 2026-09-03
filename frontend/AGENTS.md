# frontend/AGENTS.md

## Purpose

React 18 + TypeScript + Vite dashboard for Varroa (Kubernetes-native Jenkins
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
- Coverage thresholds are enforced in `vite.config.ts` (`test.coverage`):
  lines 80 / branches 80 / functions 74 / statements 80, excluding
  `src/test/` and `src/vite-env.d.ts`.
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
  editors, and the log console — never hardcode the palette.
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
- **Module ownership**: each page/component owns its own `.module.css` —
  no page imports another page's module. Small boilerplate (e.g. the
  `.page`/`.pageHead`/`.pageTitle`/`.pageDesc` header pattern) is
  duplicated per module rather than shared, matching the rest of
  `src/pages/`; a component like `SectionPage` that needs that pattern
  gets its own `SectionPage.module.css` rather than importing a page's.

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
  `Teams*`, `PluginsTab.tsx`, `FleetPlugins.tsx`). Registered in `src/App.tsx` / `src/routing.ts`.
- `src/components/` (~87 files) — shared UI building blocks (`Button`,
  `Card`, `Layout`, `Console`, `KVGrid`, `MetricCard`, `ConfigPipeline`,
  `OverlayEditor`, `ActivityTimeline`, route guards `ProtectedRoute` /
  `PermissionRoute`), each paired with a `.module.css`.
- `src/hooks/` — data-fetching hooks wrapping React Query
  (`useControllers`, `useCatalog`, `useClusters`, `useActivityFeed`,
  `useEventStream` for SSE, `usePermissions`, `useFleetPlugins`). `useControllers.ts` carries
  the hand-maintained `ControllerDetail` TypeScript interface (not generated)
  — new backend fields such as `pluginInventory`, `PluginInventorySummary`,
  and `PluginInventoryDriftEntry` must be added here in the same change or
  the frontend silently drifts from the contract.
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
  shape; shared by the brood-operation modal and the brood-schedule form).
- `src/api/` — `client.ts`, the single hand-written fetch client typed
  against `src/types/index.ts` (mirrors CRD/API shapes: `Controller`,
  `ComposedBundle`, `VarroaRole`, `JenkinsRole`, `BroodRun`, etc.).
- `src/types/` — `index.ts` (API/CRD-shaped types) and `auth.ts`.
- `src/routing.ts` — route-path helpers (`controllerRoute`, `withCluster`,
  `clusterQuery`) used to build cluster-scoped links consistently across
  pages.
