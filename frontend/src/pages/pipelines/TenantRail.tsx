import { Link } from "react-router-dom";
import { ExternalLink } from "lucide-react";
import type { PipelinesOverviewTenant, RecentRunResult } from "../../types";
import styles from "./TenantRail.module.css";

/** STRIP_CELLS is the number of result cells each rail row draws. */
const STRIP_CELLS = 10;

type CellKind = "ok" | "failed" | "running" | "idle";

/** CELL_CLASS maps a strip cell kind to its own class. The badge class `failed` is a different rule. */
const CELL_CLASS: Record<CellKind, string> = {
  ok: styles.cellOk,
  failed: styles.cellFailed,
  running: styles.cellRunning,
  idle: styles.cellIdle,
};

interface StripCell {
  kind: CellKind;
  label: string;
}

/**
 * stripCell classifies one recent run for the rail strip. A success is ok.
 * A failure, a trigger failure, or an interrupted run is failed. A run in
 * progress is running. Any other result is idle.
 */
export function stripCell(run: RecentRunResult): StripCell {
  if (run.status === "RUNNING") return { kind: "running", label: "Running" };
  if (run.status === "QUEUED") return { kind: "running", label: "Queued" };
  switch (run.result) {
    case "SUCCESS":
      return { kind: "ok", label: "Success" };
    case "FAILURE":
    case "TRIGGER_FAILED":
      return { kind: "failed", label: "Failed" };
    case "INTERRUPTED":
      return { kind: "failed", label: "Interrupted" };
    case "ABORTED":
      return { kind: "idle", label: "Aborted" };
    case "UNSTABLE":
      return { kind: "idle", label: "Unstable" };
    default:
      return { kind: "idle", label: "No result" };
  }
}

/** stripCells answers the ten cells of one row, oldest on the left and padded with idle cells. */
export function stripCells(recent: RecentRunResult[]): StripCell[] {
  const newestFirst = recent.slice(0, STRIP_CELLS);
  const cells = newestFirst.map(stripCell).reverse();
  const pad = Array.from({ length: STRIP_CELLS - cells.length }, () => ({ kind: "idle" as const, label: "No run" }));
  return [...pad, ...cells];
}

/** railMeta answers the meta line of one row. It names every cluster other than the local one. */
export function railMeta(t: PipelinesOverviewTenant, localCluster: string): string {
  let base: string;
  if (t.placement === "awaitingCell") base = "awaiting a cell";
  else if (t.runs24h === 0) base = "no runs in 24 h";
  else if (t.waitingInput > 0) base = `${t.runs24h} runs · waits for input`;
  else base = `${t.runs24h} runs · ${t.running} running`;
  const others = t.clusters.filter((c) => c !== localCluster);
  return others.length > 0 ? `${base} · ${others.join(", ")}` : base;
}

export interface TenantRailProps {
  tenants: PipelinesOverviewTenant[];
  localCluster: string;
  selected: string[];
  onSelect: (names: string[]) => void;
  /** False when one tenant is the whole view, so "All tenants" would change nothing. */
  showAll?: boolean;
}

/**
 * TenantRail lists the tenants in the order the BFF sends. It selects one
 * tenant for the feed, or every tenant. Each row names a failed badge, a
 * strip of its last results, and a meta line.
 */
export function TenantRail({ tenants, localCluster, selected, onSelect, showAll = true }: TenantRailProps) {
  const allPressed = selected.length === 0;
  return (
    <nav aria-label="Tenants" className={styles.rail}>
      <ul className={styles.list}>
        {showAll && (
          <li className={styles.item}>
            <button
              type="button"
              aria-pressed={allPressed}
              className={`${styles.select} ${allPressed ? styles.selected : ""}`}
              onClick={() => onSelect([])}
            >
              <span className={styles.name}>All tenants</span>
            </button>
          </li>
        )}
        {tenants.map((t) => {
          const pressed = selected.includes(t.name);
          return (
            <li key={t.name} className={styles.item}>
              <div className={styles.row}>
                <button
                  type="button"
                  aria-pressed={pressed}
                  className={`${styles.select} ${pressed ? styles.selected : ""}`}
                  onClick={() => onSelect(pressed ? [] : [t.name])}
                >
                  <span className={styles.head}>
                    <span className={styles.name}>{t.name}</span>
                    {t.failed24h > 0 && <span className={styles.failed}>{t.failed24h}</span>}
                  </span>
                  <span className={styles.strip}>
                    {stripCells(t.recent).map((c, i) => (
                      <span key={i} role="img" aria-label={c.label} className={`${styles.cell} ${CELL_CLASS[c.kind]}`} />
                    ))}
                  </span>
                  <span className={styles.meta}>{railMeta(t, localCluster)}</span>
                </button>
                <Link to={`/pipelines/${encodeURIComponent(t.name)}`} className={styles.open} aria-label={`Open ${t.name}`}>
                  <ExternalLink size={14} aria-hidden="true" />
                </Link>
              </div>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}
