import type { ControllerDiff, ApplyResult, ApplySectionResult } from "../types";
import { Card } from "./Card";
import styles from "./ConfigPipeline.module.css";

/** Pipeline stage data — status, hash, timestamp, and optional error. */
export interface PipelineStage {
  label: "SOURCE" | "COMPOSE" | "DESIRE" | "DELIVER" | "LIVE";
  status: string;
  hash: string;
  timestamp: string;
  error?: string;
  /** Telemetry rendered inside the stage card (DELIVER/LIVE only). */
  telemetry?: string[];
}

interface Props {
  stages: PipelineStage[];
  runningBuilds?: number;
  diff?: ControllerDiff | null;
  diffLoading?: boolean;
  diffError?: string | null;
  onFetchDiff?: () => void;
  onReload?: () => void;
  onRestart?: () => void;
  onReprovision?: () => void;
  applyHistory?: ApplyResult[];
  lastApplySections?: ApplySectionResult[];
}

export function SectionChips({ sections, size }: { sections: ApplySectionResult[]; size: "md" | "sm" }) {
  return (
    <>
      {sections.map((sec, i) => (
        <span key={i} className={size === "sm" ? styles["sec--sm"] : styles.sec} data-ok={sec.ok}>
          {sec.ok ? "✓" : "✗"} {sec.name}
        </span>
      ))}
    </>
  );
}

export function ConfigPipeline({
  stages,
  runningBuilds,
  diff,
  diffLoading,
  diffError,
  onFetchDiff,
  onReload,
  onRestart,
  onReprovision,
  applyHistory,
  lastApplySections,
}: Props) {
  const sortedHistory = applyHistory
    ? [...applyHistory].sort((a, b) => new Date(b.timestamp).getTime() - new Date(a.timestamp).getTime())
    : [];

  const failedSections = lastApplySections?.filter((s) => !s.ok) ?? [];
  const okCount = (lastApplySections?.length ?? 0) - failedSections.length;
  const total = lastApplySections?.length ?? 0;

  return (
    <Card title="Configuration pipeline">
      <div className={styles.stages}>
        {stages.map((s) => (
          <Stage key={s.label} {...s} />
        ))}
      </div>

      {/* Sections band */}
      {lastApplySections && lastApplySections.length > 0 && (
        <div className={styles.sections} data-od-id="apply-sections">
          <div className={styles.sectionsHead}>
            {okCount}/{total} applied
          </div>
          <div style={{ display: "flex", gap: 6, flexWrap: "wrap", marginBottom: 8 }}>
            <SectionChips sections={lastApplySections} size="md" />
          </div>
          {failedSections.map((sec, i) => (
            <div key={i} className={styles.secError}>
              {sec.name}: {sec.error ?? "rejected, no message returned"}
            </div>
          ))}
        </div>
      )}

      {/* Running builds */}
      {runningBuilds !== undefined && runningBuilds > 0 && (
        <div className={styles.runningBuilds}>▶ {runningBuilds} running build{runningBuilds !== 1 ? "s" : ""}</div>
      )}

      {/* Guarded actions */}
      <div className={styles.actions}>
        {onReload && <button className={styles.actionBtn} onClick={onReload}>Reload</button>}
        {onRestart && <button className={styles.actionBtn} onClick={onRestart}>Restart</button>}
        {onReprovision && <button className={styles.actionBtn} onClick={onReprovision}>Reprovision</button>}
      </div>

      {/* Diff preview */}
      {onFetchDiff && (
        <div className={styles.diffSection}>
          <button className={styles.actionBtn} onClick={onFetchDiff} disabled={diffLoading}>
            {diffLoading ? "Loading..." : "Preview diff"}
          </button>
          {diffError && <p className={styles.diffError}>{diffError}</p>}
          {diff && diff.appliedUnavailable && (
            <p className={styles.diffNotice}>last-applied unavailable (mite offline)</p>
          )}
          {diff && !diff.appliedUnavailable && diff.applied && (
            <pre className={styles.diffContent}>
              {truncate(diff.incoming.jcasc, 500)}
              {diff.incoming.jcasc && "\n\n=== items diff ===\n"}
              {truncate(diff.incoming.items, 200)}
            </pre>
          )}
          {diff && !diff.appliedUnavailable && !diff.applied && (
            <pre className={styles.diffContent}>
              {truncate(diff.incoming.jcasc, 500)}
            </pre>
          )}
        </div>
      )}

      {/* Apply history */}
      {sortedHistory.length > 0 && (
        <div className={styles.history}>
          <h4>Apply History</h4>
          {sortedHistory.map((ar, i) => (
            <div key={i} className={`${styles.historyEntry} ${ar.succeeded ? styles.success : styles.failure}`}>
              <span className={styles.historyHash}>{shortHash(ar.hash)}</span>
              <span className={styles.historyTime}>{new Date(ar.timestamp).toLocaleString()}</span>
              <span>{ar.trigger || "reconciliation"}</span>
              <span>{ar.succeeded ? "✓" : "✗"}</span>
              {ar.sections && <SectionChips sections={ar.sections} size="sm" />}
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}

function Stage({ label, status, hash, timestamp, error, telemetry }: PipelineStage) {
  return (
    <div className={styles.stage}>
      <div className={styles.stageLabel}>{label}</div>
      <div className={styles.stageState}>{status}</div>
      {hash && <div className={styles.stageHash}>{shortHash(hash)}</div>}
      {timestamp && <div className={styles.stageTs}>{new Date(timestamp).toLocaleTimeString()}</div>}
      {error && <div className={styles.stageError}>{error}</div>}
      {telemetry && telemetry.length > 0 && (
        <div className={styles.stageTelemetry}>
          {telemetry.map((t, i) => <span key={i} className={styles.telemetryItem}>{t}</span>)}
        </div>
      )}
    </div>
  );
}

function shortHash(h: string) {
  if (!h) return "";
  return h.length > 12 ? h.slice(0, 12) + "…" : h;
}

function truncate(s: string, n: number) {
  if (!s) return "";
  return s.length > n ? s.slice(0, n) + "…" : s;
}
