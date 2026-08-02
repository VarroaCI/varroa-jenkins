import { useLayoutEffect } from "react";
import { coreOf, useClusters } from "../hooks/useClusters";
import styles from "./ClusterSelector.module.css";

interface ClusterSelectorProps {
  /** Currently-selected cluster name (controlled). */
  value: string;
  /** Called with the newly-picked (or auto-seeded) cluster name. */
  onChange: (cluster: string) => void;
  /** Field label. Defaults to "Cluster". */
  label?: string;
  /** Optional helper text rendered under the select. */
  hint?: string;
}

/**
 * Shared cluster picker for the config-authoring pages. Encapsulates the
 * duplicated `useClusters` + `<select>` idiom used by the settings tabs and the
 * controller wizard:
 *  - only offers **healthy, active** clusters (you cannot author on an
 *    unreachable or draining cluster),
 *  - labels the core `${name} (core)`,
 *  - auto-seeds `value` to the core (or first active cluster) when the current
 *    selection isn't a valid active cluster — this runs even while hidden so a
 *    single-cluster install still targets the real core name, and
 *  - hides itself entirely when there are fewer than two clusters to switch
 *    between (nothing to pick).
 */
export default function ClusterSelector({ value, onChange, label = "Cluster", hint }: ClusterSelectorProps) {
  const { data: clusters } = useClusters();
  const active = (clusters ?? []).filter((c) => c.healthy && c.state === "active");

  // Auto-seed in a layout effect (not a passive effect) so the corrected
  // selection is committed before pages' passive fetch effects run — this
  // avoids an initial request against the placeholder cluster name when the
  // core isn't literally named "core".
  useLayoutEffect(() => {
    if (active.length === 0) return;
    if (active.some((c) => c.name === value)) return;
    const fallback = coreOf(active)?.name ?? active[0].name;
    if (fallback && fallback !== value) onChange(fallback);
  }, [active, value, onChange]);

  if (active.length < 2) return null;

  return (
    <div className={styles.field}>
      <label className={styles.label}>{label}</label>
      <select
        className={styles.select}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        {active.map((c) => (
          <option key={c.name} value={c.name}>
            {c.core ? `${c.name} (core)` : c.name}
          </option>
        ))}
      </select>
      {hint && <div className={styles.hint}>{hint}</div>}
    </div>
  );
}
