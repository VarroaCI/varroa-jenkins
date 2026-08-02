# design-sync notes — varroa-frontend

Repo-specific gotchas for future syncs. The DS is the React frontend in `frontend/`
(private Vite app, not a published library). Run all design-sync commands from `frontend/`.

## Build setup (non-obvious, required)

- **Synth-entry mode.** No library dist/entry — components are PascalCase exports in
  `src/components/*.tsx` styled with CSS Modules. esbuild bundles the `.module.css`
  into `_ds_bundle.css` automatically.
- **Self-symlink required.** PKG_DIR resolves to `node_modules/<pkg>`, which a private
  app never self-installs. Recreate on a fresh clone:
  `ln -sfn .. node_modules/varroa-frontend`. Without it the build dies on
  `ENOENT .../node_modules/varroa-frontend/package.json`. (Gitignored — node_modules.)
- **Tokens ship via the self-package.** `cfg.tokensPkg="varroa-frontend"` +
  `cfg.tokensGlob="src/styles/*.css"` copies `tokens.css` (the `:root --bg/--accent/...`
  defs) and `global.css` (base reset) into `tokens/`. copyTokens REQUIRES tokensPkg and
  treats tokensGlob as a single string relative to that package — a repo-relative array
  silently no-ops.
- **Jenkins skin in scope.** `public/varroa-theme.css` (DS tokens mapped onto Jenkins UI
  selectors — the branded Jenkins theme) is shipped via `cfg.cssEntry`, appended into
  `_ds_bundle.css` so it travels in the styles.css closure. The user confirmed it's a
  first-class design artifact, not irrelevant.
- **Default-exported components.** `Layout`, `ErrorBoundary`, `LoadingSpinner` use
  `export default`, which synth-entry's `export *` does NOT expose. Re-exported as named
  via `.design-sync/ds-reexports.mjs` (wired through `cfg.extraEntries`). If a new
  default-exported component is added, add it here too or it won't reach window.Varroa.
- **Brand fonts.** DS `--sans`/`--mono` stacks name "Inter" and "JetBrains Mono" but the
  repo ships neither. Shipped via `.design-sync/fonts/brand-fonts.css` + latin variable
  woff2 (from @fontsource-variable, copied into `.design-sync/fonts/`, committed),
  declared under the exact family names. `cfg.extraFonts` points at the css.

## Render check / chromium (WSL)

- Cached chromium build is `chromium_headless_shell-1223` → needs **playwright@1.60.0**
  (installed into `.ds-sync`). Repo has no playwright of its own.
- Both chromium builds are missing system libs (`libnss3`, `libnspr4`, `libasound2`) and
  sudo needs a password here. Worked around WITHOUT root: debs downloaded + extracted to
  `.ds-sync/syslibs/extracted/` (gitignored). Run validate/capture with:
  - `export LD_LIBRARY_PATH="$PWD/.ds-sync/syslibs/extracted/usr/lib/x86_64-linux-gnu:$LD_LIBRARY_PATH"`
  - `export DS_CHROMIUM_PATH=~/.cache/ms-playwright/chromium_headless_shell-1223/chrome-headless-shell-linux64/chrome-headless-shell`
  On a fresh clone these must be re-downloaded/extracted (or install the libs with sudo).

## Known render warns (triaged — not new issues)

- `[EXPORT_COLLISION] ds-reexports.mjs ... ErrorBoundary, Layout, LoadingSpinner` —
  benign. Synth-entry lists these names in the export set, but `export *` never bound the
  defaults, so the re-export bindings win at runtime (validate confirms all on
  window.Varroa, no `[BUNDLE_EXPORT]`). Renaming isn't possible — they must keep their
  real names. Ignore.
- `[TOKENS_MISSING]` ~11 vars (`--color-text`, `--color-border`, `--color-surface`,
  `--text-1`, `--color-code*`, `--color-hover`, `--color-primary`, ...) referenced by
  `BundleSelector`, `LoadingSpinner`, `ConfigPipeline` module CSS but defined NOWHERE in
  the repo — legacy/dead refs from a partial token-rename; the app falls back too.
  Non-blocking, not ours to fix.
- `[FONT_MISSING] "Cascadia Code"` — tertiary fallback in the `--mono` stack after
  JetBrains Mono (which we ship). Accepted system substitute; never reached in practice.

## Preview authoring

- **`cfg.provider` = `PreviewProvider`** (`.design-sync/PreviewProvider.tsx`, exposed via
  extraEntries). Wraps every preview in the app's real providers — MemoryRouter +
  QueryClientProvider + ThemeProvider + ToastProvider + AuthProvider — and **seeds the
  query cache** (`['me']` = a mock admin user "Ada Bramwell", `['permissions']` =
  `{"*":{"*":true}}`, `['auth-config']` = oidc) so router/auth/permission-dependent
  components render fully populated instead of hitting a non-existent API. This is what
  makes Sidebar/Topbar/Layout render their real chrome.
- **CSS comment gotcha (cost a debug cycle):** in `closure-extras.css`, never write the
  literal `*/` inside a CSS comment (e.g. `--color-*/--text-1`) — it closes the comment
  early and silently breaks the `:root` alias block, so the legacy tokens go undefined and
  e.g. the spinner renders invisible. Keep `*` and `/` separated.
- **Authored (15, all graded good):** Button, Card, StatusPill, MetricCard, Pulse,
  LoadingSpinner, KVGrid, Tabs, BundleHealthBadge, Console, Sidebar, Topbar, Layout,
  ConfigPipeline, ObservabilityPanel.
- **Floor cards (deliberate, render-check-clean):**
  - `CommandPalette` — renders `null` until opened via ⌘K; interaction-only, can't render statically.
  - `ProfileMenu` — its value is the dropdown (interaction-gated); default render is just the avatar (already visible inside Topbar).
  - `BundleSelector` — driven by a `useComposedBundles` fetch; would need a seeded `['composed-bundles', ns]` mock to populate (not done — author later if wanted).
  - `ErrorBoundary`, `ProtectedRoute` — infrastructure (error catcher / routing guard), nothing visual to show.
- **`ToastProvider` excluded** from cards via `componentSrcMap: {"ToastProvider": null}` —
  it's a context provider with no standalone visual. Still on window.Varroa (used by PreviewProvider).
- **Card modes:** Layout `single` (1180x700 shell), Sidebar `single` (300x660), Topbar &
  ConfigPipeline & MetricCard `column` (full-width-per-story, avoids `[GRID_OVERFLOW]`).

## Re-sync risks (watch list)

- `node_modules/varroa-frontend` self-symlink and `.ds-sync/syslibs/` are gitignored —
  both must be recreated on a fresh clone (see above).
- `.design-sync/ds-reexports.mjs` is hand-maintained against the set of default-exported
  components — drifts if components switch export style or new default exports are added.
- Brand-font woff2 are pinned latin-subset snapshots of @fontsource-variable; a fontsource
  bump won't reflect unless re-copied.
- `cfg.cssEntry` is the Jenkins skin (not the component stylesheet — those come from
  esbuild CSS-modules); don't "fix" it to point at a dist stylesheet.
