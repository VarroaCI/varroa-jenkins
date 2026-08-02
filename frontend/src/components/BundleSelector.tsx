import { useState, useMemo } from "react";
import { useComposedBundles } from "../hooks/useCatalog";
import type { ComposedBundlePhase } from "../types";
import styles from "./BundleSelector.module.css";

// ---- BundleHealthBadge ----

const PHASE_META: Record<ComposedBundlePhase, { label: string; className: string }> = {
  Ready: { label: "Ready", className: "phaseReady" },
  Drifted: { label: "Drifted", className: "phaseDrifted" },
  Invalid: { label: "Invalid", className: "phaseInvalid" },
  Pending: { label: "Pending", className: "phasePending" },
};

export function BundleHealthBadge({ phase }: { phase?: ComposedBundlePhase }) {
  if (!phase) return null;
  const meta = PHASE_META[phase];
  if (!meta) return <span className={styles.phaseBadge}>{phase}</span>;
  return (
    <span className={`${styles.phaseBadge} ${styles[meta.className]}`}>
      {meta.label}
    </span>
  );
}

// ---- BundleSelector ----

interface BundleSelectorProps {
  cluster: string;
  namespace: string;
  value: string | null;
  onChange: (name: string | null) => void;
  disabled?: boolean;
}

export function BundleSelector({
  cluster,
  namespace,
  value,
  onChange,
  disabled,
}: BundleSelectorProps) {
  const { data, isLoading, error } = useComposedBundles(cluster, namespace);
  const [search, setSearch] = useState("");

  const bundles = useMemo(() => {
    if (!data?.items) return [];
    const q = search.toLowerCase().trim();
    if (!q) return data.items;
    return data.items.filter((b) =>
      b.metadata.name.toLowerCase().includes(q)
    );
  }, [data, search]);

  const selectedBundle = useMemo(
    () => data?.items?.find((b) => b.metadata.name === value) ?? null,
    [data, value]
  );

  const handleChange = (e: React.ChangeEvent<HTMLSelectElement>) => {
    const v = e.target.value;
    onChange(v === "" ? null : v);
  };

  const handleClear = () => onChange(null);

  return (
    <div className={styles.selector}>
      {/* Search + select */}
      <div className={styles.searchRow}>
        <input
          className={styles.searchInput}
          type="text"
          placeholder="Search bundles..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          disabled={disabled}
        />
      </div>

      <div className={styles.selectRow}>
        <select
          className={styles.select}
          value={value ?? ""}
          onChange={handleChange}
          disabled={disabled || isLoading}
        >
          <option value="">-- No bundle selected --</option>
          {bundles.map((b) => (
            <option key={b.metadata.name} value={b.metadata.name}>
              {b.spec.displayName || b.metadata.name}
            </option>
          ))}
        </select>

        {value && !disabled && (
          <button
            type="button"
            className={styles.clearBtn}
            onClick={handleClear}
            title="Clear selection"
          >
            &times;
          </button>
        )}
      </div>

      {/* Loading / error states */}
      {isLoading && (
        <div className={styles.infoText}>Loading bundles...</div>
      )}
      {error && (
        <div className={styles.errorText}>
          Failed to load bundles: {error.message}
        </div>
      )}
      {!isLoading && !error && bundles.length === 0 && (
        <div className={styles.infoText}>
          {search ? "No bundles match your search." : "No bundles found in this namespace."}
        </div>
      )}

      {/* Selected bundle health */}
      {selectedBundle && selectedBundle.status && (
        <div className={styles.healthSection}>
          <div className={styles.healthRow}>
            <span className={styles.healthLabel}>Status</span>
            <BundleHealthBadge phase={selectedBundle.status.phase} />
          </div>

          {/* Errors list */}
          {selectedBundle.status.phase !== "Ready" &&
            selectedBundle.status.errors &&
            selectedBundle.status.errors.length > 0 && (
              <div className={styles.issueList}>
                {selectedBundle.status.errors.map((err, i) => (
                  <div key={`err-${i}`} className={styles.errorItem}>
                    ⛔ {err}
                  </div>
                ))}
              </div>
            )}

          {/* Warnings list */}
          {selectedBundle.status.phase !== "Ready" &&
            selectedBundle.status.warnings &&
            selectedBundle.status.warnings.length > 0 && (
              <div className={styles.issueList}>
                {selectedBundle.status.warnings.map((w, i) => (
                  <div key={`warn-${i}`} className={styles.warningItem}>
                    ⚠ {w}
                  </div>
                ))}
              </div>
            )}
        </div>
      )}
    </div>
  );
}
