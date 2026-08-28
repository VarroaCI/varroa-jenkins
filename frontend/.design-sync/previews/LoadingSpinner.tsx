import { LoadingSpinner } from "varroa-frontend";

export function Default() {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12, alignItems: "center", padding: 24 }}>
      <LoadingSpinner />
      <span style={{ color: "var(--text-3)", fontSize: 13 }}>Loading controllers…</span>
    </div>
  );
}
