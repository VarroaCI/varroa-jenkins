import { useEffect, useState } from "react";
import { getVersionProfiles } from "../api/client";
import type { VersionProfileDetail, VersionProfileCondition } from "../types";
import LoadingSpinner from "../components/LoadingSpinner";
import ClusterSelector from "../components/ClusterSelector";
import styles from "./settings.module.css";

function findCondition(
  conditions: VersionProfileCondition[],
  type: string,
): VersionProfileCondition | undefined {
  return conditions.find((c) => c.type === type);
}

export default function SettingsVersionsTab() {
  const [profiles, setProfiles] = useState<VersionProfileDetail[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [selectedCluster, setSelectedCluster] = useState("core");

  useEffect(() => {
    setLoading(true);
    setError(null);
    getVersionProfiles(selectedCluster)
      .then((p) => setProfiles(p))
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  }, [selectedCluster]);

  if (loading) return <LoadingSpinner />;
  if (error) return <div className={styles.errorBanner}>Error: {error}</div>;
  if (!profiles || profiles.length === 0) {
    return <p className={styles.muted}>No version profiles installed</p>;
  }

  const now = new Date();

  return (
    <div>
      <p className={`${styles.muted} ${styles.pageNote}`}>
        Version profiles are read-only in this UI and scoped to the selected cluster. Each pins a plugin set and optional
        JCasC overlay to a Jenkins version or LTS line.
      </p>
      <ClusterSelector value={selectedCluster} onChange={setSelectedCluster} />
      <div className={styles.profileTableWrap}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Version</th><th>Channel</th><th>Plugins</th><th>Plugin set</th><th>JCasC</th><th>Materialized</th>
            </tr>
          </thead>
          <tbody>
            {profiles.map((p) => {
              const metadataOnly = !p.pluginSetRef;
              const eolPast = p.eol ? new Date(p.eol) < now : false;
              const ready = findCondition(p.conditions, "PluginSetReady");
              const mismatch = findCondition(p.conditions, "LockJcascMismatch");
              const mismatchActive = mismatch?.status === "True";

              return (
                <tr key={p.name}>
                  <td>
                    <div className={styles.profileBadges}>
                      <span className={styles.profileVersion}>{p.version}</span>
                      {p.recommended && (
                        <span
                          className={`${styles.tableBadge} ${styles.recommended}`}
                        >
                          recommended
                        </span>
                      )}
                      {p.eol && (
                        <span
                          className={`${styles.tableBadge} ${eolPast ? styles.eol : ""}`}
                        >
                          EOL {p.eol}
                        </span>
                      )}
                    </div>
                  </td>
                  <td>
                    <span className={styles.tableBadge}>
                      {p.channel}
                    </span>
                  </td>
                  <td>{metadataOnly ? "—" : p.pluginCount ?? "—"}</td>
                  <td>
                    {metadataOnly ? (
                      "—"
                    ) : ready?.status === "True" ? (
                      <span className={styles.ready} title={ready.message}>
                        Ready
                      </span>
                    ) : (
                      <span className={styles.notReady} title={ready?.message}>
                        Error{ready?.reason ? ` (${ready.reason})` : ""}
                      </span>
                    )}
                  </td>
                  <td>
                    <div className={styles.profileBadges}>
                      {p.hasJcasc ? (
                        <span className={styles.tableBadge}>
                          overlay
                        </span>
                      ) : (
                        "—"
                      )}
                      {mismatchActive && (
                        <span
                          className={`${styles.tableBadge} ${styles.eol}`}
                          title={mismatch?.message}
                        >
                          lock mismatch
                        </span>
                      )}
                    </div>
                  </td>
                  <td>
                    {p.contentRef ? (
                      <code className={styles.profileCode}>{p.contentRef}</code>
                    ) : (
                      "—"
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}
