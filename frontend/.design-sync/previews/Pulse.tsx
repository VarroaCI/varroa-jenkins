import { Pulse } from "varroa-frontend";

export function States() {
  return (
    <div style={{ display: "flex", gap: 28, alignItems: "center" }}>
      <span style={{ display: "inline-flex", gap: 8, alignItems: "center", color: "var(--text)" }}>
        <Pulse active /> Live
      </span>
      <span style={{ display: "inline-flex", gap: 8, alignItems: "center", color: "var(--text-3)" }}>
        <Pulse active={false} /> Idle
      </span>
    </div>
  );
}

export function Sizes() {
  return (
    <div style={{ display: "flex", gap: 20, alignItems: "center" }}>
      <Pulse active size={8} />
      <Pulse active size={12} />
      <Pulse active size={18} />
    </div>
  );
}
