import { Activity, Clock, Gauge, Hourglass } from "lucide-react";
import { MetricCard } from "../../components/MetricCard";
import type { PipelinesOverview, PipelinesOverviewTotals } from "../../types";
import { formatDuration } from "./JobTree";
import styles from "./FleetCounters.module.css";

/** RunsTile is the run count of the last 24 h, with the change from the prior 24 h. */
function RunsTile({ totals: t }: { totals: PipelinesOverviewTotals }) {
  const delta = t.runs24h - t.runsPrior24h;
  const deltaText = delta > 0 ? `+${delta}` : delta < 0 ? `−${Math.abs(delta)}` : "0";
  return (
    <MetricCard
      label="Runs"
      value={t.runs24h}
      icon={<Activity size={16} aria-hidden="true" />}
      sub={`${deltaText} vs prior 24 h`}
    />
  );
}

/** PassRateTile is the pass rate of the last 24 h, with its result counts. */
function PassRateTile({ totals: t }: { totals: PipelinesOverviewTotals }) {
  const passRate = t.passRate === undefined ? "—" : `${Math.round(t.passRate * 100)}%`;
  return (
    <MetricCard
      label="Pass rate"
      value={passRate}
      icon={<Gauge size={16} aria-hidden="true" />}
      accent="ok"
      sub={`${t.passed} passed · ${t.failed} failed · ${t.interrupted} interrupted`}
    />
  );
}

/** RunningTile is the count of runs in progress, with the queue and its median wait. */
function RunningTile({ totals: t }: { totals: PipelinesOverviewTotals }) {
  return (
    <MetricCard
      label="Running now"
      value={t.running}
      icon={<Hourglass size={16} aria-hidden="true" />}
      accent="info"
      sub={`${t.queued} queued · median wait ${formatDuration(t.medianQueueWaitMs)}`}
    />
  );
}

/** DurationTile is the median duration of the last 24 h, with its p90. */
function DurationTile({ totals: t }: { totals: PipelinesOverviewTotals }) {
  return (
    <MetricCard
      label="Median duration"
      value={formatDuration(t.medianDurationMs)}
      icon={<Clock size={16} aria-hidden="true" />}
      sub={`p90 ${formatDuration(t.p90DurationMs)}`}
    />
  );
}

/** FleetCounters renders the four tiles of the fleet overview over the last 24 h. */
export function FleetCounters({ overview }: { overview: PipelinesOverview }) {
  return (
    <div className={styles.grid}>
      <RunsTile totals={overview.totals} />
      <PassRateTile totals={overview.totals} />
      <RunningTile totals={overview.totals} />
      <DurationTile totals={overview.totals} />
    </div>
  );
}
