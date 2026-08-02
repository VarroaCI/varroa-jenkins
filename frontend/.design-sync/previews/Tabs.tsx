import { useState } from "react";
import { Tabs } from "varroa-frontend";

const TABS = [
  { id: "overview", label: "Overview" },
  { id: "config", label: "Config" },
  { id: "logs", label: "Logs" },
  { id: "rbac", label: "RBAC" },
];

export function Interactive() {
  const [active, setActive] = useState("overview");
  return (
    <div style={{ maxWidth: 460 }}>
      <Tabs tabs={TABS} activeTab={active} onSelect={setActive} />
      <div style={{ padding: "16px 4px", color: "var(--text-2)" }}>
        Showing <strong style={{ color: "var(--text)" }}>{active}</strong> for smoke-main.
      </div>
    </div>
  );
}

export function SecondSelected() {
  const [active, setActive] = useState("config");
  return (
    <div style={{ maxWidth: 460 }}>
      <Tabs tabs={TABS} activeTab={active} onSelect={setActive} />
    </div>
  );
}
