import type { FleetRunRow } from "../../types";
import { age } from "../activityTimeline.util";
import { foldSecondLine, foldTitle } from "./runFold";
import { runActorText, runTiming } from "./runRowText";
import { RunGlyphMark } from "./RunGlyphMark";
import styles from "./RunRow.module.css";

export interface RunFoldRowProps {
  /** The folded runs, newest first. The row shows the newest run's state. */
  runs: FleetRunRow[];
  now: number;
  /** The cluster of the caller's own controller. A run on another cluster shows its cluster. */
  localCluster?: string;
  /** True when the fold shows its runs below the row. */
  expanded: boolean;
  onToggle: () => void;
}

/**
 * RunFoldRow renders a fold of consecutive runs as one table row. Its title
 * button shows or hides the folded runs. The row has no menu, so each run
 * keeps its own actions.
 */
export function RunFoldRow({ runs, now, localCluster, expanded, onToggle }: RunFoldRowProps) {
  const newest = runs[0];
  const started = newest.startedAt ?? newest.queuedAt ?? newest.createdAt;

  return (
    <tr className={styles.row}>
      <td className={styles.statusCell}>
        <RunGlyphMark run={newest} />
      </td>
      <td className={styles.runCell}>
        <button type="button" className={styles.foldToggle} aria-expanded={expanded} onClick={onToggle}>
          <span aria-hidden="true">{expanded ? "▼ " : "▶ "}</span>
          {foldTitle(runs)}
        </button>
        <div className={styles.second}>{foldSecondLine(runs)}</div>
      </td>
      <td className={styles.tenantCell}>
        <span className={styles.tenantName}>{newest.tenant}</span>
        {localCluster !== undefined && newest.cluster !== localCluster && (
          <span className={styles.chip}>{newest.cluster}</span>
        )}
      </td>
      <td className={styles.actorCell}>{runActorText(newest) ?? ""}</td>
      <td className={styles.branchCell}>{newest.branch ? <span className={styles.chip}>{newest.branch}</span> : null}</td>
      <td className={styles.startedCell}>{age(started, { now })}</td>
      <td className={styles.durationCell}>
        <span className={styles.timing}>{runTiming(newest, now)}</span>
      </td>
    </tr>
  );
}
