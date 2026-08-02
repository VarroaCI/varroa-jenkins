import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { getCatalogItem } from "../api/client";
import { clusterQuery } from "../routing";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import type {
  CatalogItem,
  CatalogItemLockPins,
  CatalogVariable,
} from "../types";
import { VERDICT_LABEL, isWarningVerdict } from "../lib/compat";
import styles from "./CatalogItemDetail.module.css";

const TYPE_ICONS: Record<string, string> = {
  podtemplate: "━",
  plugin: "◆",
  item: "○",
  jcasc: "⚙",
  rbac: "⚇",
  "pipeline-template": "▶",
};

export default function CatalogItemDetail() {
  const { name, namespace } = useParams<{ name: string; namespace: string }>();
  const { cluster, ready } = useConfigurationCluster();
  const [item, setItem] = useState<CatalogItem | null>(null);
  // Lock pins are cluster state joined by the BFF at read time; they are
  // deliberately not stored on the item, where they would go stale.
  const [lockPins, setLockPins] = useState<CatalogItemLockPins[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    if (!cluster || !name || !namespace) return;
    setLoading(true);
    setError(null);
    getCatalogItem(cluster, namespace, name)
      .then((resp) => {
        setItem(resp.item);
        setLockPins(resp.lockPins ?? []);
      })
      .catch((e) => setError(e.message))
      .finally(() => setLoading(false));
  }, [cluster, name, namespace]);

  if (ready && !cluster) return <NoAccessibleClusters />;
  if (loading) {
    return <div className={styles.loadingBanner}>Loading item...</div>;
  }

  if (error) {
    return (
      <div>
        <Link to={`/catalog${clusterQuery(cluster)}`} className={styles.backLink}>
          &larr; Back to Catalog
        </Link>
        <div className={styles.errorBanner}>Error: {error}</div>
      </div>
    );
  }

  if (!item) return null;

  const spec = item.spec;
  const status = item.status;
  const vars: CatalogVariable[] = spec.variables || [];
  const closure = status?.closure ?? [];
  const compat = status?.compat ?? [];

  return (
    <div className={styles.page}>
      <Link to={`/catalog${clusterQuery(cluster)}`} className={styles.backLink}>
        &larr; Back to Catalog
      </Link>

      <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 4 }}>
        <span style={{ fontSize: 22 }}>{TYPE_ICONS[spec.type] || "○"}</span>
        <h2 className={styles.title}>{spec.displayName || item.metadata.name}</h2>
      </div>
      <p className={styles.subtitle}>
        Catalog item &middot; {spec.type}
      </p>

      {/* Validation badge */}
      {status && (
        <div style={{ marginBottom: 20 }}>
          {status.valid ? (
            <span className={styles.validBadge}>&#x2713; Valid</span>
          ) : (
            <span className={styles.invalidBadge}>
              &#x26A0; Invalid{status.message ? `: ${status.message}` : ""}
            </span>
          )}
        </div>
      )}

      {/* Details section */}
      <div className={styles.section}>
        <h3 className={styles.sectionTitle}>Details</h3>
        <div className={styles.detailGrid}>
          <span className={styles.detailLabel}>Name</span>
          <span className={styles.mono}>{item.metadata.name}</span>
          <span className={styles.detailLabel}>Namespace</span>
          <span>{item.metadata.namespace || "default"}</span>
          <span className={styles.detailLabel}>Source</span>
          <span>{spec.sourceRef}</span>
          <span className={styles.detailLabel}>Type</span>
          <span>{spec.type}</span>
          <span className={styles.detailLabel}>Path</span>
          <span className={styles.mono}>{spec.path}</span>
          {spec.version && (
            <>
              <span className={styles.detailLabel}>Version</span>
              <span className={styles.mono}>{spec.version}</span>
            </>
          )}
          {status?.contentHash && (
            <>
              <span className={styles.detailLabel}>Content Hash</span>
              <span className={styles.mono}>{status.contentHash.substring(0, 16)}...</span>
            </>
          )}
          {status?.observedRevision && (
            <>
              <span className={styles.detailLabel}>Revision</span>
              <span className={styles.mono}>{status.observedRevision}</span>
            </>
          )}
          <span className={styles.detailLabel}>Created</span>
          <span>
            {item.metadata.creationTimestamp
              ? new Date(item.metadata.creationTimestamp).toLocaleDateString()
              : "-"}
          </span>
        </div>
      </div>

      {/* Description */}
      {spec.description && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Description</h3>
          <p style={{ fontSize: "0.92rem", lineHeight: 1.6, color: "var(--text-2)" }}>
            {spec.description}
          </p>
        </div>
      )}

      {/* Tags */}
      {spec.tags && spec.tags.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Tags</h3>
          <div className={styles.tags}>
            {spec.tags.map((tag) => (
              <span key={tag} className={styles.tag}>{tag}</span>
            ))}
          </div>
        </div>
      )}

      {/* Variables */}
      {vars.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>
            Variables ({vars.length})
          </h3>
          {vars.map((v) => (
            <div key={v.name} className={styles.variableCard}>
              <div className={styles.variableName}>
                {v.name}
                {v.required && (
                  <span style={{ color: "var(--bad-text)", marginLeft: 4 }}>*</span>
                )}
                {v.type && (
                  <span className={styles.tag} style={{ marginLeft: 8 }}>{v.type}</span>
                )}
              </div>
              {v.default && (
                <div className={styles.variableMeta}>
                  Default: <span className={styles.mono}>{v.default}</span>
                </div>
              )}
              {v.description && (
                <div className={styles.variableDesc}>{v.description}</div>
              )}
              {v.allowedValues && v.allowedValues.length > 0 && (
                <div className={styles.variableMeta}>
                  Allowed: <span className={styles.mono}>{v.allowedValues.join(", ")}</span>
                </div>
              )}
            </div>
          ))}
        </div>
      )}

      {/* Pinned dependency closure. Every column is read straight from
          status.closure — the browser re-derives nothing, having neither the
          store annotations nor the solver. */}
      {closure.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Pinned closure ({closure.length})</h3>
          <div className={styles.tableScroll}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Plugin</th>
                  <th>Version</th>
                  <th>Origin</th>
                  <th>From</th>
                  <th>Minimum</th>
                  {lockPins.map((lp) => (
                    <th key={lp.profile}>{lp.profile} lock</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {closure.map((entry) => (
                  <tr key={entry.artifactId}>
                    <td className={styles.mono}>{entry.artifactId}</td>
                    <td className={styles.mono}>{entry.version}</td>
                    <td>{entry.direct ? "direct" : "transitive"}</td>
                    <td>{entry.provenance ?? "-"}</td>
                    <td className={styles.mono}>{entry.minimum || "-"}</td>
                    {lockPins.map((lp) => {
                      const pinned = lp.pins[entry.artifactId];
                      // No key means this profile\u2019s lock does not pin the
                      // plugin at all, which is distinct from pinning it equal.
                      if (pinned === undefined) {
                        return (
                          <td key={lp.profile} className={styles.lockUnpinned}>
                            not pinned
                          </td>
                        );
                      }
                      const diverges = pinned !== entry.version;
                      return (
                        <td
                          key={lp.profile}
                          className={diverges ? styles.lockDiverges : styles.mono}
                          title={
                            diverges
                              ? "This profile pins a different version; selecting this item will trip the provisioning plugin-version conflict check."
                              : undefined
                          }
                        >
                          {pinned}
                          {diverges ? " \u2260" : ""}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Per-profile compatibility. Advisory in every case: nothing here blocks
          selection, composition, or provisioning. */}
      {compat.length > 0 && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Compatibility ({compat.length} profiles)</h3>
          <p className={styles.advisoryNote}>
            Advisory only. These verdicts never block selecting this item into a bundle or
            provisioning a controller.
          </p>
          <div className={styles.tableScroll}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Profile</th>
                  <th>Jenkins</th>
                  <th>Verdict</th>
                  <th>Detail</th>
                </tr>
              </thead>
              <tbody>
                {compat.map((c) => (
                  <tr key={c.profile}>
                    <td>{c.profile}</td>
                    <td className={styles.mono}>{c.jenkinsVersion || "-"}</td>
                    <td>
                      <span className={styles.compatBadge} data-verdict={c.verdict}>
                        {isWarningVerdict(c.verdict) ? "\u26A0 " : ""}
                        {VERDICT_LABEL[c.verdict]}
                      </span>
                    </td>
                    <td className={styles.compatMessage}>{c.message || "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* YAML content preview */}
      {status?.content && (
        <div className={styles.section}>
          <h3 className={styles.sectionTitle}>Content</h3>
          <pre className={styles.yamlBlock}>{status.content}</pre>
        </div>
      )}
    </div>
  );
}
