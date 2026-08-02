import { Layout } from "varroa-frontend";

// The full application shell: fixed Sidebar + Topbar around the routed content
// area (empty here — no active route). Shows how the chrome composes.
export function AppShell() {
  return (
    <div style={{ height: 680, background: "var(--bg)" }}>
      <Layout />
    </div>
  );
}
