import type { ControllerListItem } from "../../hooks/useControllers";
import styles from "./PhaseBar.module.css";

const PHASE_BUCKETS = ["Connected", "Running", "Provisioning", "Hibernated", "Failed"] as const;

export interface PhaseCount {
  phase: string;
  count: number;
}

/**
 * phaseCounts counts controllers per phase bucket. A phase outside the buckets
 * counts as "Other". Zero buckets are dropped.
 */
export function phaseCounts(cs: ControllerListItem[]): PhaseCount[] {
  const byPhase = new Map<string, number>();
  for (const c of cs) {
    const bucket = (PHASE_BUCKETS as readonly string[]).includes(c.phase) ? c.phase : "Other";
    byPhase.set(bucket, (byPhase.get(bucket) ?? 0) + 1);
  }
  return [...PHASE_BUCKETS, "Other"]
    .map((phase) => ({ phase, count: byPhase.get(phase) ?? 0 }))
    .filter((x) => x.count > 0);
}

/**
 * PhaseBar draws the phase distribution as one stacked bar. Its counts are
 * carried by the accessible name, because the bar shows no legend.
 */
export function PhaseBar({ counts }: { counts: PhaseCount[] }) {
  const label = "Phase distribution: " + counts.map((x) => `${x.count} ${x.phase}`).join(", ");
  return (
    <div className={styles.phaseBar} role="img" aria-label={label}>
      {counts.map((x) => (
        <span key={x.phase} className={styles.phaseSeg} data-phase={x.phase} style={{ flexGrow: x.count }} />
      ))}
    </div>
  );
}
