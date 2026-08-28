import type { ControllerObservability, ObservabilitySource, ObservabilitySourceStatus } from "../types";
import { Card } from "./Card";
import { Pulse } from "./Pulse";
import styles from "./ObservabilityPanel.module.css";

const SOURCE_BADGE: Record<ObservabilitySourceStatus, { tone: string; label: string }> = {
  "not-configured": { tone: "badgeNotConfigured", label: "Not configured" },
  intended: { tone: "badgeIntended", label: "Declared" },
  configured: { tone: "badgeConfigured", label: "Configured" },
  exposed: { tone: "badgeExposed", label: "Exposed" },
  integrated: { tone: "badgeIntegrated", label: "Integrated" },
  degraded: { tone: "badgeDegraded", label: "Degraded" },
  unavailable: { tone: "badgeUnavailable", label: "Unavailable" },
  unknown: { tone: "badgeUnknown", label: "Unknown" },
};

const PROVIDER_LABELS: Record<string, string> = {
  "jenkins-api": "Jenkins API",
  prometheus: "Prometheus",
  opentelemetry: "OpenTelemetry",
};

interface Props {
  observability?: ControllerObservability;
}

export function ObservabilityPanel({ observability }: Props) {
  if (!observability) {
    return (
      <Card title="Observability">
        <div className={styles.emptyText}>No observability data available yet.</div>
      </Card>
    );
  }

  const { sources, capabilities, level, levelName, freshness, warnings, summary } = observability;
  const capSet = new Set(capabilities);
  const sortedSources = [...sources].sort((a, b) => a.provider.localeCompare(b.provider));

  return (
    <div>
      <Card
        title="Observability"
        headerRight={
          <span className={styles.levelBadge}>
            Level {level}: {levelName}
          </span>
        }
      >
        <FreshnessBanner freshness={freshness} />

        {!!warnings?.length && (
          <div className={styles.warnings}>
            {warnings.map((warning) => warning.message).join(" ")}
          </div>
        )}

        <SourceCards sources={sortedSources} />

        {capSet.has("jenkins.health") && <CapabilityCard title="Jenkins API" body="Live Jenkins health is available through the mite-managed API probe." />}
        {capSet.has("jenkins.jobs.summary") && (
          <div className={styles.metricGrid}>
            <MetricChip label="Total jobs" value={summary?.totalJobs ?? "-"} />
            <MetricChip label="Running builds" value={summary?.runningBuilds ?? "-"} />
          </div>
        )}
        {capSet.has("jenkins.builds.recent") && (
          <RecentBuildsCard recentBuilds={summary?.recentBuilds ?? []} />
        )}
        {capSet.has("jenkins.metrics.endpoint") && (
          <CapabilityCard title="Prometheus Endpoint" body="Jenkins exposes a Prometheus scrape endpoint for this controller." />
        )}
        {capSet.has("jenkins.builds.trends") && capSet.has("jenkins.metrics.query") && (
          <CapabilityCard title="Build Trends" body="Prometheus-backed build trend metrics are queryable for this controller." />
        )}
        {capSet.has("jenkins.queue.metrics") && (
          <CapabilityCard title="Queue Metrics" body="Queue metrics are available from the cached observability model." />
        )}
        {capSet.has("jenkins.executors.metrics") && (
          <CapabilityCard title="Executor Metrics" body="Executor capacity metrics are available from the cached observability model." />
        )}
        {capSet.has("jenkins.traces.exporting") && (
          <CapabilityCard
            title="OpenTelemetry"
            body={capSet.has("jenkins.traces.query") ? "Trace export and trace navigation are configured." : "Trace exporting is configured, but trace query links are not available yet."}
          />
        )}
      </Card>
    </div>
  );
}

function FreshnessBanner({ freshness }: { freshness: ControllerObservability["freshness"] }) {
  if (!freshness.observedAt && !freshness.miteTTL) {
    return null;
  }

  return (
    <div className={styles.freshnessBanner} data-stale={freshness.stale}>
      {freshness.stale ? "Observability data may be stale." : "Observability data is cached."}
      {freshness.observedAt && <> Last observed: {freshness.observedAt}.</>}
      {freshness.miteTTL ? <> TTL: {freshness.miteTTL}s.</> : null}
    </div>
  );
}

function SourceCards({ sources }: { sources: ObservabilitySource[] }) {
  if (sources.length === 0) {
    return (
      <div className={styles.noSources}>
        No observability sources detected.
      </div>
    );
  }

  return (
    <div className={styles.sourceCards}>
      {sources.map((s) => {
        const badge = SOURCE_BADGE[s.status];
        const hintText = s.hints ? Object.values(s.hints).filter(Boolean).join(" • ") : "";
        return (
          <div key={s.provider} className={styles.sourceCard}>
            <Pulse active={s.status === "exposed" || s.status === "integrated"} size={6} />
            <span className={styles.providerLabel}>{PROVIDER_LABELS[s.provider] ?? s.provider}</span>
            <span className={styles[badge.tone as keyof typeof styles]}>{badge.label}</span>
            {hintText && <span className={styles.hintText}>{hintText}</span>}
            {s.error && <span className={styles.sourceError}>{s.error}</span>}
          </div>
        );
      })}
    </div>
  );
}

function MetricChip({ label, value }: { label: string; value: string | number }) {
  return (
    <div className={styles.metricChip}>
      <div className={styles.metricChipLabel}>{label}</div>
      <div className={styles.metricChipValue}>{value}</div>
    </div>
  );
}

function CapabilityCard({ title, body }: { title: string; body: string }) {
  return (
    <div className={styles.infoBox}>
      <div className={styles.infoBoxTitle}>{title}</div>
      <div>{body}</div>
    </div>
  );
}

function RecentBuildsCard({ recentBuilds }: { recentBuilds: NonNullable<ControllerObservability["summary"]>["recentBuilds"] }) {
  return (
    <div className={styles.infoBox}>
      <div className={styles.recentBuildsTitle}>Recent Builds</div>
      {recentBuilds && recentBuilds.length > 0 ? (
        <div className={styles.recentBuildsGrid}>
          {recentBuilds.map((build) => (
            <div key={`${build.jobName}-${build.buildNumber}`}>
              <span className={styles.providerLabel}>{build.jobName}</span>
              <span> #{build.buildNumber}</span>
              <span className={styles.buildStatusText}> {build.status}</span>
            </div>
          ))}
        </div>
      ) : (
        <div className={styles.buildStatusText}>No cached recent build data.</div>
      )}
    </div>
  );
}
