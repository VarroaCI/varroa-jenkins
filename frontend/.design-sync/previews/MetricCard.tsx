import { MetricCard } from "varroa-frontend";

export function Dashboard() {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(4, minmax(150px, 1fr))", gap: 14 }}>
      <MetricCard label="Controllers" value={12} sub="across 3 groups" accent="accent" icon="◆" />
      <MetricCard label="Connected" value={9} sub="75% healthy" accent="ok" icon="●" />
      <MetricCard label="Provisioning" value={2} sub="in progress" accent="warn" icon="◐" />
      <MetricCard label="Failed" value={1} sub="needs attention" accent="bad" icon="▲" />
    </div>
  );
}

export function Accents() {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(3, minmax(150px, 1fr))", gap: 14 }}>
      <MetricCard label="Builds / hr" value="248" accent="info" icon="↑" />
      <MetricCard label="Queue depth" value={6} accent="honey" icon="≡" />
      <MetricCard label="Avg duration" value="3m 12s" accent="default" />
    </div>
  );
}
