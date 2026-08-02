import { ObservabilityPanel } from "varroa-frontend";

export function Empty() {
  return (
    <div style={{ maxWidth: 520 }}>
      <ObservabilityPanel />
    </div>
  );
}

export function Integrated() {
  return (
    <div style={{ maxWidth: 520 }}>
      <ObservabilityPanel
        observability={{
          level: 3,
          levelName: "Integrated",
          capabilities: [
            "jenkins.health",
            "jenkins.jobs.summary",
            "jenkins.builds.recent",
            "jenkins.metrics.endpoint",
          ],
          freshness: { observedAt: "2026-06-17T10:42:30Z", stale: false },
          sources: [
            { provider: "jenkins-api", status: "integrated" },
            { provider: "prometheus", status: "exposed" },
            { provider: "opentelemetry", status: "configured" },
          ],
          summary: { totalJobs: 142, runningBuilds: 3, recentBuilds: [] },
          warnings: [],
        }}
      />
    </div>
  );
}
