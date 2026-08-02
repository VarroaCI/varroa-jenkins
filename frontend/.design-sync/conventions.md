# Varroa design system — how to build with it

Varroa is a Kubernetes operator dashboard for managing Jenkins controllers. The look is a
warm "honey / wax comb" palette: cream backgrounds, honey-orange accent, soft status tints.
All components ship compiled on `window.Varroa.*` and render real React.

## Setup & wrapping

Most components are self-contained leaves — no provider needed: **Button, Card, StatusPill,
MetricCard, Pulse, LoadingSpinner, KVGrid, Tabs, BundleHealthBadge, Console,
ObservabilityPanel, ConfigPipeline**. Just import and render them.

The **app-chrome** components — **Sidebar, Topbar, Layout, ProfileMenu, CommandPalette** —
read app context and must be wrapped (outermost → in): a **React Router** (`BrowserRouter`),
a **QueryClientProvider** (`@tanstack/react-query`), then `ThemeProvider`, `ToastProvider`,
and `AuthProvider`. `Layout` renders the full shell (Sidebar + Topbar + routed `<Outlet/>`).
Without these wrappers those components throw or render blank.

**Theming is attribute-driven, not prop-driven.** Set attributes on `<html>`:
`data-theme="dark"` (default light) and `data-accent="honey|rust|pollen|propolis"` (default
honey). Every token re-resolves automatically — never hard-code colors.

## Styling idiom: props + tokens, NOT class names

Components are styled internally with CSS Modules (scoped class names). **Do not try to
restyle a component by passing CSS classes** — change its appearance through its **props**:
`Button` (`variant: default|primary|ghost`, `size: default|sm`), `MetricCard`
(`accent: default|ok|warn|bad|info|accent|honey`), `StatusPill` (`phase`),
`BundleHealthBadge` (`phase`), `Pulse` (`active`, `size`).

For **your own** layout and markup around the components, use the design tokens — global CSS
custom properties on `:root` (light + dark + accent variants all defined):

| Family | Tokens |
|---|---|
| Surfaces | `--bg`, `--surface`, `--surface-2`, `--surface-3` |
| Borders | `--border`, `--border-strong` |
| Text | `--text`, `--text-2`, `--text-3` |
| Brand | `--accent`, `--accent-soft`, `--accent-strong`, `--honey`, `--honey-soft` |
| Status | `--ok`, `--warn`, `--bad`, `--idle`, `--info` (+ `*-soft` tints) |
| Shape | `--radius`, `--radius-sm` |
| Elevation | `--shadow-sm`, `--shadow-md`, `--shadow-lg` |
| Type | `--sans` (Inter), `--mono` (JetBrains Mono) |

Example: `style={{ background: "var(--surface)", color: "var(--text-2)", border: "1px solid
var(--border)", borderRadius: "var(--radius)", padding: 16 }}`.

## Where the truth lives

- Tokens: `tokens/tokens.css` (definitions) + `tokens/global.css` (base reset). Read these
  before styling.
- Per-component API: `<Name>.d.ts`. Usage examples: `<Name>.prompt.md`.
- A branded **Jenkins theme** (Varroa tokens mapped onto Jenkins UI selectors) ships inside
  `_ds_bundle.css` — relevant only if mocking Jenkins' own chrome, not Varroa screens.

## One idiomatic build

```tsx
import { Card, MetricCard, StatusPill } from "window.Varroa"; // exposed as window.Varroa.*

function ControllerSummary() {
  return (
    <div style={{ display: "grid", gap: 16, background: "var(--bg)", padding: 24 }}>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(3, 1fr)", gap: 14 }}>
        <MetricCard label="Controllers" value={12} accent="accent" />
        <MetricCard label="Connected" value={9} sub="75% healthy" accent="ok" />
        <MetricCard label="Failed" value={1} accent="bad" />
      </div>
      <Card title="smoke-main" headerRight={<StatusPill phase="Connected" size="sm" />}>
        <span style={{ color: "var(--text-2)" }}>Namespace jenkins · 1/1 ready</span>
      </Card>
    </div>
  );
}
```
