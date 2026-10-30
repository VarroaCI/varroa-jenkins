import { useState, type KeyboardEvent } from "react";
import { Link } from "react-router-dom";
import { MoreVertical } from "lucide-react";
import type { FleetRunRow } from "../../types";
import { age } from "../activityTimeline.util";
import { runHref, runSecondLine, runTiming } from "./runRowText";
import { RunGlyphMark } from "./RunGlyphMark";
import styles from "./RunRow.module.css";

export interface RunRowProps {
  run: FleetRunRow;
  now: number;
  /** The cluster of the caller's own controller. A run on another cluster shows its cluster. */
  localCluster?: string;
  onRerun: (r: FleetRunRow) => void;
  onCancel: (r: FleetRunRow) => void;
}

/**
 * RunRow renders one run of the fleet feed as one table row. Its kebab
 * holds Open, and the write actions only when the BFF reports canWrite.
 */
export function RunRow({ run, now, localCluster, onRerun, onCancel }: RunRowProps) {
  const [menuOpen, setMenuOpen] = useState(false);
  const href = runHref(run.tenant, run.jobPath ?? "", run.runId);
  const started = run.startedAt ?? run.queuedAt ?? run.createdAt;
  const canRerun = run.canWrite && run.status === "COMPLETED";
  const canCancel = run.canWrite && (run.status === "QUEUED" || run.status === "RUNNING");

  const closeMenu = () => setMenuOpen(false);
  const onMenuKey = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape") closeMenu();
  };

  return (
    <tr className={styles.row}>
      <td className={styles.statusCell}>
        <RunGlyphMark run={run} />
      </td>
      <td className={styles.runCell}>
        <Link to={href} className={styles.title}>
          {run.title}
        </Link>
        <div className={styles.second}>{runSecondLine(run)}</div>
      </td>
      <td className={styles.tenantCell}>
        <span className={styles.tenantName}>{run.tenant}</span>
        {localCluster !== undefined && run.cluster !== localCluster && (
          <span className={styles.chip}>{run.cluster}</span>
        )}
      </td>
      <td className={styles.actorCell}>{run.actor ?? ""}</td>
      <td className={styles.branchCell}>{run.branch ? <span className={styles.chip}>{run.branch}</span> : null}</td>
      <td className={styles.startedCell}>{age(started, { now })}</td>
      <td className={styles.durationCell}>
        <span className={styles.timing}>{runTiming(run, now)}</span>
        <div className={styles.kebabWrap} onKeyDown={onMenuKey}>
          <button
            type="button"
            className={styles.kebab}
            aria-label={`Actions for ${run.title}`}
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            onClick={() => setMenuOpen((open) => !open)}
          >
            <MoreVertical size={16} aria-hidden="true" />
          </button>
          {menuOpen && (
            <div role="menu" className={styles.menu}>
              <Link role="menuitem" to={href} className={styles.menuItem} onClick={closeMenu}>
                Open
              </Link>
              {canRerun && (
                <button
                  type="button"
                  role="menuitem"
                  className={styles.menuItem}
                  onClick={() => {
                    closeMenu();
                    onRerun(run);
                  }}
                >
                  Re-run
                </button>
              )}
              {canCancel && (
                <button
                  type="button"
                  role="menuitem"
                  className={styles.menuItem}
                  onClick={() => {
                    closeMenu();
                    onCancel(run);
                  }}
                >
                  Cancel
                </button>
              )}
            </div>
          )}
        </div>
      </td>
    </tr>
  );
}
