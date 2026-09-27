import type { BindingFederationState, VarroaRoleBindingStatus } from "../types";
import { age } from "./activityTimeline.util";
import styles from "./FederationStatus.module.css";

const STATE_ORDER: BindingFederationState[] = ["failed", "skipped", "federated"];

const STATE_CLASS: Record<BindingFederationState, string> = {
  federated: styles.federated,
  skipped: styles.skipped,
  failed: styles.failed,
};

/** Counts the hive entries of a binding status by state. */
export function federationCounts(status?: VarroaRoleBindingStatus): Record<BindingFederationState, number> {
  const counts: Record<BindingFederationState, number> = { federated: 0, skipped: 0, failed: 0 };
  for (const entry of status?.federation ?? []) {
    if (entry.state in counts) counts[entry.state] += 1;
  }
  return counts;
}

/**
 * The one-line federation summary of a binding: the non-zero hive count of
 * each state, failed first. A binding with no entries reads "Not reported".
 */
export function FederationSummary({ status }: { status?: VarroaRoleBindingStatus }) {
  const counts = federationCounts(status);
  const parts = STATE_ORDER.filter((state) => counts[state] > 0);
  if (parts.length === 0) {
    return <span className={styles.none}>Not reported</span>;
  }
  return (
    <span className={styles.summary}>
      {parts.map((state) => (
        <span key={state} className={STATE_CLASS[state]}>
          {counts[state]} {state}
        </span>
      ))}
    </span>
  );
}

/** The read-only per-hive federation table of one binding. */
export function FederationStatusTable({ status }: { status?: VarroaRoleBindingStatus }) {
  const entries = status?.federation ?? [];
  return (
    <div className={styles.block}>
      <div className={styles.title}>Hive federation</div>
      {entries.length === 0 ? (
        <p className={styles.none}>
          No hive reports this binding. The core BFF writes one row for each hive cluster.
        </p>
      ) : (
        <div className={styles.scroll}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Cluster</th>
                <th>State</th>
                <th>Reason</th>
                <th>Message</th>
                <th>Since</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((entry) => (
                <tr key={entry.cluster}>
                  <td className={styles.mono}>{entry.cluster}</td>
                  <td className={STATE_CLASS[entry.state]}>{entry.state}</td>
                  <td className={styles.mono}>{entry.reason ?? ""}</td>
                  <td>{entry.message ?? ""}</td>
                  <td title={entry.lastTransitionTime}>{age(entry.lastTransitionTime)}</td>
                </tr>
              ))}
            </tbody>
        </table>
        </div>
      )}
    </div>
  );
}
