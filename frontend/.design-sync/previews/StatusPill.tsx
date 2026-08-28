import { StatusPill } from "varroa-frontend";

export function Phases() {
  return (
    <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
      <StatusPill phase="Connected" />
      <StatusPill phase="Running" />
      <StatusPill phase="Provisioning" />
      <StatusPill phase="Pending" />
      <StatusPill phase="Stopped" />
      <StatusPill phase="Failed" />
    </div>
  );
}

export function Small() {
  return (
    <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
      <StatusPill phase="Connected" size="sm" />
      <StatusPill phase="Provisioning" size="sm" />
      <StatusPill phase="Failed" size="sm" />
    </div>
  );
}
