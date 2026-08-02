import styles from "./ControllerDetail.module.css";

interface Props {
  ctrl: { pluginInventory?: import("../hooks/useControllers").PluginInventorySummary };
}

const CLASS_LABELS: Record<string, string> = {
  bootstrap: "Bootstrap",
  declared: "Declared",
  "jenkins-supplied": "Jenkins-supplied",
  dependency: "Dependency",
  "optional-dependency": "Optional dep",
  unmanaged: "Unmanaged",
};

const CLASS_ORDER = ["bootstrap", "declared", "jenkins-supplied", "dependency", "optional-dependency", "unmanaged"];

export default function PluginsTab({ ctrl }: Props) {
  const pi = ctrl.pluginInventory;

  if (!pi) {
    return (
      <div className={styles.pluginsEmpty}>
        No plugin inventory available yet — the mite has not reported one.
      </div>
    );
  }

  const stateLabel = pi.stale
    ? "Stale"
    : pi.degraded
      ? "Degraded"
      : pi.optionalEdgesDropped
        ? "Indeterminate"
        : "Active";

  return (
    <div className={styles.pluginsPanel}>
      <div className={styles.pluginsHeader}>
        <span className={styles.pluginsHeaderLabel}>Plugin Inventory</span>
        <span className={`${styles.pluginsState} ${pi.stale ? styles.pluginsStateStale : ""}`}>
          {stateLabel}
        </span>
      </div>

      {(pi.stale || pi.degraded || pi.optionalEdgesDropped) && (
        <div className={styles.pluginsBanner}>
          {pi.stale &&
            "Inventory is stale — mite disconnected, collection failed, or read model lost. " +
              "Drift detection is paused."}
          {!pi.stale &&
            pi.degraded &&
            "Inventory source is filesystem (Jenkins API unreachable). " +
              "Flags are unobservable; drift detection is advisory only."}
          {!pi.stale &&
            !pi.degraded &&
            pi.optionalEdgesDropped &&
            "Optional dependency edges were dropped to fit the transport budget. " +
              "Drift is indeterminate."}
        </div>
      )}

      <div className={styles.pluginsCounts}>
        {pi.counts ? (
          CLASS_ORDER.map((cls) => {
            const n = pi.counts![cls];
            if (n === undefined) return null;
            return (
              <span key={cls} className={styles.pluginsCount}>
                <span className={styles.pluginsCountLabel}>{CLASS_LABELS[cls] ?? cls}</span>
                <span className={styles.pluginsCountValue}>{n}</span>
              </span>
            );
          })
        ) : (
          <span className={styles.pluginsCount}>
            <span className={styles.pluginsCountLabel}>Total</span>
            <span className={styles.pluginsCountValue}>{pi.total}</span>
          </span>
        )}
      </div>

      {pi.drift && pi.drift.length > 0 && (
        <section className={styles.pluginsSection}>
          <h3 className={styles.pluginsSectionTitle}>Drift ({pi.drift.length}{pi.driftTruncated ? "+" : ""})</h3>
          <div className={styles.pluginDiff}>
            {pi.drift.map((d) => (
              <div key={d.name} className={styles.pluginDiffRemove}>
                {d.name} {d.version}
                {d.class && ` — ${CLASS_LABELS[d.class] ?? d.class}`}
              </div>
            ))}
          </div>
        </section>
      )}

      {pi.versionDrift && pi.versionDrift.length > 0 && (
        <section className={styles.pluginsSection}>
          <h3 className={styles.pluginsSectionTitle}>Version Drift ({pi.versionDrift.length}{pi.driftTruncated ? "+" : ""})</h3>
          <div className={styles.pluginDiff}>
            {pi.versionDrift.map((d) => (
              <div key={d.name} className={styles.pluginDiffChange}>
                {d.name} {d.version}
                {d.verdict && ` — ${d.verdict}`}
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  );
}
