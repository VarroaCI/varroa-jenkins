import { useParams, Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { CloudRain, CloudSun, Sun, type LucideIcon } from "lucide-react";
import { listPipelines, listRunners } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { ForbiddenPage, GenericErrorPage, NotFoundPage, RecordStoreDisabledPage } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import { age } from "../../components/activityTimeline.util";
import type { PipelineHealth, PipelineSummary, RunnerEntry, RunningRun } from "../../types";
import styles from "./TeamHome.module.css";

const enc = (s: string) => encodeURIComponent(s);

function jobHref(team: string, jobPath: string): string {
  return `/pipelines/${enc(team)}/${enc(jobPath)}`;
}

function runHref(team: string, jobPath: string, runId: string): string {
  return `${jobHref(team, jobPath)}/${enc(runId)}`;
}

/** formatDuration renders a millisecond span the way the mockup does: the
 * two coarsest non-zero units, dropping finer ones. */
function formatDuration(ms: number | undefined): string {
  if (ms === undefined || ms < 0) return "—";
  const totalSec = Math.round(ms / 1000);
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

const HEALTH_GLYPH: Record<"clear" | "cloudy" | "rain", { Icon: LucideIcon; label: string; cls: string }> = {
  clear: { Icon: Sun, label: "Clear: none of the last five runs failed", cls: styles.healthClear },
  cloudy: { Icon: CloudSun, label: "Cloudy: one or two of the last five runs failed", cls: styles.healthCloudy },
  rain: { Icon: CloudRain, label: "Rain: three or more of the last five runs failed", cls: styles.healthRain },
};

/**
 * HealthGlyph renders the weather glyph naming its own state, so a screen
 * reader announces the state rather than the image (design D8). A job that
 * has never run carries health "unknown" and renders no glyph.
 */
function HealthGlyph({ health }: { health: PipelineHealth }) {
  if (health === "unknown") return null;
  const { Icon, label, cls } = HEALTH_GLYPH[health];
  return (
    <span className={`${styles.healthCell} ${cls}`} role="img" aria-label={label} title={label}>
      <Icon aria-hidden="true" size={18} />
    </span>
  );
}

/** Sparkline draws the last-8-durations trend as one inline SVG polyline. No
 * charting package is added for this (design D8, rejected alternative 9). */
function Sparkline({ values }: { values: number[] }) {
  if (values.length === 0) return null;
  const w = 64;
  const h = 20;
  const max = Math.max(...values, 1);
  const min = Math.min(...values, 0);
  const range = max - min || 1;
  const step = values.length > 1 ? w / (values.length - 1) : 0;
  const points = values
    .map((v, i) => `${(i * step).toFixed(1)},${(h - ((v - min) / range) * h).toFixed(1)}`)
    .join(" ");
  return (
    <svg
      className={styles.sparkline}
      width={w}
      height={h}
      viewBox={`0 0 ${w} ${h}`}
      role="img"
      aria-label="Duration trend, last 8 builds"
    >
      <polyline points={points} fill="none" stroke="currentColor" strokeWidth={1.5} />
    </svg>
  );
}

function RunningTile({ team, run }: { team: string; run: RunningRun }) {
  const started = run.startedAt ?? run.queuedAt;
  const elapsedMs = started ? Date.now() - Date.parse(started) : 0;
  const pct = run.typicalDurationMs
    ? Math.max(0, Math.min(100, (elapsedMs / run.typicalDurationMs) * 100))
    : 0;
  return (
    <Link to={runHref(team, run.jobPath, run.runId)} className={styles.tile}>
      <div className={styles.tileHead}>
        <span className={styles.tileJob}>{run.jobPath}</span>
        <span className={styles.tileBuild}>{run.buildNumber ? `#${run.buildNumber}` : "Queued"}</span>
      </div>
      <div className={styles.tileStage}>{run.currentStage ?? "Queued"}</div>
      <div className={styles.tileMeta}>
        <span>{run.branch ?? "—"}</span>
        <span>{run.scmRevision ? run.scmRevision.slice(0, 7) : "—"}</span>
      </div>
      {run.changeSummary && <div className={styles.tileCommitMsg}>{run.changeSummary}</div>}
      <div className={styles.progressTrack}>
        <div className={styles.progressFill} style={{ width: `${pct}%` }} />
      </div>
      <div className={styles.tileTimes}>
        <span>{started ? age(started) : "queued"}</span>
        <span>usual {formatDuration(run.typicalDurationMs)}</span>
      </div>
    </Link>
  );
}

function PipelineRow({ team, item }: { team: string; item: PipelineSummary }) {
  return (
    <tr>
      <td className={styles.nameCell}>
        <Link to={jobHref(team, item.jobPath)}>{item.jobPath}</Link>
      </td>
      <td>
        <HealthGlyph health={item.health} />
      </td>
      <td>
        {item.lastBuild ? (
          <Link to={runHref(team, item.jobPath, item.lastBuild.runId)}>
            #{item.lastBuild.buildNumber ?? "—"}
          </Link>
        ) : (
          <span className={styles.dim}>—</span>
        )}
      </td>
      <td>{item.lastBuild?.branch ?? <span className={styles.dim}>—</span>}</td>
      <td>
        <div className={styles.durationCell}>
          {item.durationTrendMs && item.durationTrendMs.length > 0 && <Sparkline values={item.durationTrendMs} />}
          <span className={styles.lastDuration}>{formatDuration(item.lastBuild?.durationMs)}</span>
        </div>
      </td>
      <td>
        {item.lastBuild?.endedAt ? (
          age(item.lastBuild.endedAt)
        ) : item.lastBuild?.startedAt ? (
          age(item.lastBuild.startedAt)
        ) : (
          <span className={styles.dim}>—</span>
        )}
      </td>
    </tr>
  );
}

/**
 * RunnerRow renders one external runner's inventory row. It never reads
 * status.token: the dashboard is not a place a door credential appears
 * (design D12). C9 owns the Jenkins UI block, so this row carries no
 * Jenkins link.
 */
function RunnerRow({ item }: { item: RunnerEntry }) {
  const labels = item.spec.labels ?? [];
  return (
    <tr>
      <td className={styles.nameCell}>{item.name}</td>
      <td>{item.spec.launcher}</td>
      <td>{labels.length > 0 ? labels.join(", ") : <span className={styles.dim}>—</span>}</td>
      <td>{item.status.cell || <span className={styles.dim}>—</span>}</td>
      <td>
        <span className={item.status.connected ? styles.connectedYes : styles.connectedNo}>
          {item.status.connected ? "Connected" : "Not connected"}
        </span>
      </td>
      <td>{item.status.lastSeen ? age(item.status.lastSeen) : <span className={styles.dim}>—</span>}</td>
    </tr>
  );
}

export default function TeamHome() {
  const { team } = useParams<{ team: string }>();
  const { data, isLoading, error } = useQuery({
    queryKey: ["pipelines", team],
    queryFn: () => listPipelines(team!),
    enabled: !!team,
  });
  const runnersQuery = useQuery({
    queryKey: ["runners", team],
    queryFn: () => listRunners(team!),
    enabled: !!team,
  });

  if (isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading pipelines…</div>
      </div>
    );
  }

  if (error instanceof ApiError && error.status === 403) return <ForbiddenPage />;
  if (error instanceof ApiError && error.status === 404) return <NotFoundPage />;
  if (error instanceof ApiError && error.status === 503) return <RecordStoreDisabledPage />;
  if (error) return <GenericErrorPage />;
  if (!data) return <NotFoundPage />;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div className={styles.pageTitle}>{team}</div>
        <div className={styles.pageDesc}>Pipelines for this team</div>
      </div>

      <div className={styles.sectionTitle}>Running now</div>
      {data.truncated && (
        <p className={styles.truncatedNote}>
          This view is bounded and may not show every running build.
        </p>
      )}
      {data.runningNow.length === 0 ? (
        <div className={styles.empty}>Nothing is running.</div>
      ) : (
        <div className={styles.tileGrid}>
          {data.runningNow.map((run) => (
            <RunningTile key={run.runId} team={team!} run={run} />
          ))}
        </div>
      )}

      <div className={styles.sectionTitle}>Pipelines</div>
      {data.items.length === 0 ? (
        <div className={styles.empty}>No pipelines yet.</div>
      ) : (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Pipeline</th>
                <th>Health</th>
                <th>Last build</th>
                <th>Branch</th>
                <th>Duration, last 8</th>
                <th>When</th>
              </tr>
            </thead>
            <tbody>
              {data.items.map((item) => (
                <PipelineRow key={item.jobPath} team={team!} item={item} />
              ))}
            </tbody>
          </table>
        </Card>
      )}

      <div className={styles.sectionTitle}>Runners</div>
      {runnersQuery.isLoading ? (
        <div className={styles.loading}>Loading runners…</div>
      ) : runnersQuery.isError ? (
        <div className={styles.empty}>Unable to load runners.</div>
      ) : !runnersQuery.data || runnersQuery.data.length === 0 ? (
        <div className={styles.empty}>No runners registered for this team yet.</div>
      ) : (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Name</th>
                <th>Launcher</th>
                <th>Labels</th>
                <th>Cell</th>
                <th>Connected</th>
                <th>Last seen</th>
              </tr>
            </thead>
            <tbody>
              {runnersQuery.data.map((item) => (
                <RunnerRow key={item.name} item={item} />
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </div>
  );
}
