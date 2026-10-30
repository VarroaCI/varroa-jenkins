import { Activity, Clock, Gauge, Hourglass } from "lucide-react";
import { MetricCard } from "../../components/MetricCard";
import type { PipelinesOverview } from "../../types";
import { formatDuration } from "./JobTree";
import styles from "./FleetCounters.module.css";

/** FleetCounters renders the four tiles of the fleet overview over the last 24 h. */
export function FleetCounters({ overview }: { overview: PipelinesOverview }) {
  const t = overview.totals;
  const delta = t.runs24h - t.runsPrior24h;
  const deltaText = delta > 0 ? `+${delta}` : delta < 0 ? `−${Math.abs(delta)}` : "0";
  const passRate = t.passRate === undefined ? "—" : `${Math.round(t.passRate * 100)}%`;

  return (
    <div className={styles.grid}>
      <MetricCard
        label="Runs"
        value={t.runs24h}
        icon={<Activity size={16} aria-hidden="true" />}
        sub={`${deltaText} vs prior 24 h`}
      />
      <MetricCard
        label="Pass rate"
        value={passRate}
        icon={<Gauge size={16} aria-hidden="true" />}
        accent="ok"
        sub={`${t.passed} passed · ${t.failed} failed · ${t.interrupted} interrupted`}
      />
      <MetricCard
        label="Running now"
        value={t.running}
        icon={<Hourglass size={16} aria-hidden="true" />}
        accent="info"
        sub={`${t.queued} queued · median wait ${formatDuration(t.medianQueueWaitMs)}`}
      />
      <MetricCard
        label="Median duration"
        value={formatDuration(t.medianDurationMs)}
        icon={<Clock size={16} aria-hidden="true" />}
        sub={`p90 ${formatDuration(t.p90DurationMs)}`}
      />
    </div>
  );
}
