import { Link } from "react-router-dom";
import { useControllers } from "../../hooks/useControllers";
import { ApiError, isRecordStoreUnreachable } from "../../hooks/useApi";
import { RecordStoreDisabledPage, RecordStoreUnreachablePage, RecoveryState } from "../../components/RecoveryState";
import type { PipelinesOverview, PipelinesOverviewHour } from "../../types";
import { usePipelinesOverview } from "../pipelines/usePipelinesOverview";
import { PhaseBar, phaseCounts } from "./PhaseBar";
import { failedHoursLabel, jenkinsLine, nextScheduledLine, runningParts, totalsParts } from "./dayText";
import { useNow } from "./useNow";
import styles from "./DayPanel.module.css";

export interface DayPanelProps {
  /** Whether the caller reads pipelines. It shows the runs block. */
  canPipelines: boolean;
  /** Whether the caller reads controllers. It shows the Jenkins row. */
  canControllers: boolean;
}

/**
 * DayPanel summarizes the last 24 hours: the runs and their hourly bars, what
 * runs now, and the Jenkins controllers. Each block reads on its own and shows
 * its own error state.
 */
export function DayPanel({ canPipelines, canControllers }: DayPanelProps) {
  return (
    <section className={styles.panel} aria-labelledby="overview-day">
      <h2 id="overview-day" className={styles.title}>
        Last 24 hours
      </h2>
      {canPipelines && <PipelinesBlock />}
      {canControllers && <JenkinsRow />}
    </section>
  );
}

function PipelinesBlock() {
  const q = usePipelinesOverview(true);
  const now = useNow(30_000);

  if (q.isPending) return <p className={styles.note}>Loading runs…</p>;
  if (q.isError) {
    if (isRecordStoreUnreachable(q.error)) {
      return <RecordStoreUnreachablePage compact onRetry={() => void q.refetch()} />;
    }
    if (q.error instanceof ApiError && q.error.status === 503) return <RecordStoreDisabledPage compact />;
    return (
      <RecoveryState
        compact
        kind="error"
        title="Unable to load pipelines"
        message="Varroa could not load the pipelines overview."
        onRetry={() => void q.refetch()}
      />
    );
  }
  const ov = q.data;
  if (ov.tenants.length === 0) {
    return (
      <RecoveryState
        compact
        kind="403"
        title="No pipelines access"
        message="No Tenant grants you pipelines access. Ask a Tenant admin to add you."
      />
    );
  }
  return <DayBody ov={ov} now={now} />;
}

function DayBody({ ov, now }: { ov: PipelinesOverview; now: number }) {
  const hourly = ov.hourly ?? [];
  const running = ov.running ?? [];
  const totals = totalsParts(ov.totals);
  const run = runningParts(running, now);
  return (
    <>
      <div className={styles.hist}>
        {totals ? (
          <p className={styles.totals}>
            <b>{totals.runs}</b> runs · <b>{totals.passed}</b> passed ·{" "}
            <b className={totals.failed > 0 ? styles.failCount : undefined}>{totals.failed}</b> failed · median{" "}
            {totals.median}
          </p>
        ) : (
          <p className={styles.totals}>No runs in the last 24 hours.</p>
        )}
        <HourBars hourly={hourly} />
        <div className={styles.axis}>
          <span>24 h ago</span>
          <span>12 h</span>
          <span>now</span>
        </div>
      </div>
      <div className={styles.srow}>
        <span className={styles.key}>Running</span>
        <span className={styles.body}>
          {running.length > 0 && <span className={styles.pulse} aria-hidden="true" />}
          {run.name ? (
            <>
              <b className={styles.runName}>{run.name}</b>
              {run.detail}
            </>
          ) : (
            <span className={styles.muted}>{run.detail}</span>
          )}
          <span className={styles.muted}>{nextScheduledLine(ov.totals.queued, ov.nextScheduled)}</span>
        </span>
        <Link to="/pipelines" className={styles.link}>
          Open pipelines
        </Link>
      </div>
    </>
  );
}

function HourBars({ hourly }: { hourly: PipelinesOverviewHour[] }) {
  const max = Math.max(1, ...hourly.map((h) => h.runs));
  return (
    <div className={styles.bars} role="img" aria-label={failedHoursLabel(hourly)}>
      {hourly.map((h, i) => (
        <span
          key={i}
          className={styles.bar}
          data-failed={h.failed > 0 ? "true" : undefined}
          style={{ height: h.runs === 0 ? "0%" : `${Math.max(8, Math.round((h.runs / max) * 100))}%` }}
        />
      ))}
    </div>
  );
}

function JenkinsRow() {
  const q = useControllers(true);
  const cs = q.data ?? [];
  let body;
  if (q.isPending) {
    body = <span className={styles.muted}>Loading controllers…</span>;
  } else if (q.isError) {
    body = (
      <span>
        Varroa could not load the controllers.{" "}
        <button type="button" className={styles.retry} onClick={() => void q.refetch()}>
          Retry
        </button>
      </span>
    );
  } else if (cs.length === 0) {
    body = <span className={styles.muted}>No controllers yet.</span>;
  } else {
    const counts = {
      connected: cs.filter((c) => c.miteConnected).length,
      total: cs.length,
      asleep: cs.filter((c) => c.phase === "Hibernated").length,
      stopped: cs.filter((c) => c.phase === "Stopped").length,
    };
    body = (
      <>
        <span>{jenkinsLine(counts)}</span>
        <PhaseBar counts={phaseCounts(cs)} />
      </>
    );
  }
  return (
    <div className={styles.srow}>
      <span className={styles.key}>Jenkins</span>
      <span className={styles.body}>{body}</span>
      <Link to="/controllers" className={styles.link}>
        Controllers
      </Link>
    </div>
  );
}
