import { useEffect, useState, useRef } from "react";
import { getUpdateCenterStatus, getUpdateCenterPlugins, uploadUpdateCenterPlugin } from "../api/client";
import type {
  UpdateCenterStatus,
  UpdateCenterPlugins,
  UpdateCenterCondition,
  UpdateCenterUploadResult,
  UpdateCenterUnresolvedDependency,
} from "../types";
import LoadingSpinner from "../components/LoadingSpinner";
import { canDoGlobal } from "../hooks/usePermissions";
import { useAuth } from "../context/AuthContext";
import { ApiError } from "../hooks/useApi";
import styles from "./settings.module.css";

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const val = bytes / Math.pow(1024, i);
  return `${val.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export default function SettingsUpdateCenterTab() {
  const [status, setStatus] = useState<UpdateCenterStatus | null>(null);
  const [plugins, setPlugins] = useState<UpdateCenterPlugins | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [query, setQuery] = useState("");
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const [reloadKey, setReloadKey] = useState(0);

  // Upload state. `result` holds either a dry-run plan or a committed upload —
  // the shape is the same, and `dryRun` distinguishes them.
  const { permissions } = useAuth();
  const canUpload = canDoGlobal(permissions, "updatecenter", "upload");
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<UpdateCenterUploadResult | null>(null);
  const [rejection, setRejection] = useState<UploadRejectionView | null>(null);

  async function runUpload(dryRun: boolean) {
    if (!file) return;
    setBusy(true);
    setResult(null);
    setRejection(null);
    try {
      const res = await uploadUpdateCenterPlugin(file, dryRun);
      setResult(res);
      if (!dryRun) {
        // A committed upload changes what the store holds, so the inventory is
        // refetched rather than left stale.
        setReloadKey((k) => k + 1);
      }
    } catch (e) {
      setRejection(toRejectionView(e));
    } finally {
      setBusy(false);
    }
  }

  // Fetch status on mount.
  useEffect(() => {
    setLoading(true);
    setError(null);
    getUpdateCenterStatus()
      .then((s) => {
        setStatus(s);
        if (!s.enabled) {
          setPlugins(null);
        }
      })
      .catch((e) => {
        setError(e instanceof Error ? e.message : String(e));
        setStatus(null);
      })
      .finally(() => setLoading(false));
  }, []);

  // Debounced re-fetch of plugins when query changes, only when enabled.
  useEffect(() => {
    if (!status?.enabled) return;
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      getUpdateCenterPlugins(query)
        .then(setPlugins)
        .catch((e) => {
          setError(e instanceof Error ? e.message : String(e));
        });
    }, 250);
    return () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
    };
  }, [query, status?.enabled, reloadKey]);

  if (loading) return <LoadingSpinner />;
  if (error) return <div className={styles.errorBanner}>Error: {error}</div>;
  if (!status?.enabled) {
    return <p className={styles.muted}>Update Center is not enabled on this cluster.</p>;
  }

  // ---- Status card ----

  const phaseBadgeClass = status.phase === "Error" || status.phase === "Degraded"
    ? styles.eol
    : status.phase === "Ready"
      ? styles.recommended
      : "";

  return (
    <div>
      {/* Status card */}
      <div className={styles.identityNotice}>
        <div className={styles.row12}>
          <div className={styles.grow}>
            <div className={styles.row12}>
              <span className={`${styles.tableBadge} ${phaseBadgeClass}`}>{status.phase}</span>
              <span className={styles.smallMuted}>{status.pluginCount} plugins · {formatBytes(status.storeBytes)}</span>
            </div>
          </div>
        </div>

        <div className={styles.stack12} />

        {/* Conditions */}
        {status.conditions.map((c: UpdateCenterCondition) => (
          <div key={c.type} className={styles.row} style={{ marginBottom: 4 }}>
            <span className={`${styles.tableBadge} ${c.status === "True" ? styles.recommended : styles.eol}`}>
              {c.type}
            </span>
            <span className={styles.smallMuted} title={c.message}>
              {c.status === "True" ? "OK" : c.reason || c.status}
            </span>
          </div>
        ))}

        <div className={styles.stack12} />

        {/* Metadata fields */}
        <div className={styles.smallMuted}>
          <div>Storage: {status.storageType}</div>
          <div>Pull-through: {status.pullThroughEnabled ? "enabled" : "disabled"}</div>
          <div>Last sync: {status.lastSyncTime ? new Date(status.lastSyncTime).toLocaleString() : "never"}</div>
        </div>
      </div>

      {/* Upload plugin */}
      {canUpload && (
        <>
          <h3 className={styles.sectionTitle}>Upload plugin</h3>
          <div className={styles.uploadPanel}>
            <div className={styles.uploadRow}>
              <input
                type="file"
                accept=".hpi,.jpi"
                aria-label="Plugin artifact"
                className={styles.uploadFileInput}
                onChange={(e) => {
                  setFile(e.target.files?.[0] ?? null);
                  setResult(null);
                  setRejection(null);
                }}
              />
              <button
                type="button"
                className={styles.uploadBtn}
                disabled={!file || busy}
                onClick={() => runUpload(true)}
              >
                Preview closure
              </button>
              <button
                type="button"
                className={styles.uploadBtn}
                disabled={!file || busy}
                onClick={() => runUpload(false)}
              >
                Upload
              </button>
            </div>

            {busy && <LoadingSpinner />}

            {result && !result.dryRun && (
              <div className={styles.uploadSuccess}>
                Stored {result.plugin.name}@{result.plugin.version}
                {result.packRef ? ` as ${result.packRef}` : ""}.
              </div>
            )}

            {result && (
              <>
                <div className={styles.uploadSummary}>
                  {result.plugin.name}@{result.plugin.version} —{" "}
                  {result.closure.length} mandatory {result.closure.length === 1 ? "dependency" : "dependencies"},{" "}
                  {result.closure.filter((c) => c.fetched).length}{" "}
                  {result.dryRun ? "would be downloaded" : "downloaded"}
                </div>
                {result.closure.length > 0 && (
                  <div className={styles.profileTableWrap}>
                    <table className={styles.table}>
                      <thead>
                        <tr>
                          <th>Dependency</th><th>Minimum</th><th>Status</th><th>Resolved</th><th>Source</th>
                        </tr>
                      </thead>
                      <tbody>
                        {result.closure.map((c) => (
                          <tr key={c.name}>
                            <td>{c.name}</td>
                            <td>{c.min}</td>
                            <td><span className={styles.tableBadge}>{c.status}</span></td>
                            <td>{c.resolvedVersion || "—"}</td>
                            <td>{c.source || "—"}</td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
                {result.optionalDependencies.length > 0 && (
                  <div className={styles.uploadNote}>
                    Optional dependencies (never resolved):{" "}
                    {result.optionalDependencies.map((o) => `${o.name} >= ${o.min}`).join(", ")}
                  </div>
                )}
                {result.warnings.map((warn, i) => (
                  <div key={i} className={styles.uploadRemediation}>
                    [{warn.code}] {warn.plugin}: {warn.message}
                  </div>
                ))}
                <div className={styles.uploadNote}>
                  Storing a plugin is not installing it: a bundle that installs this plugin must
                  enumerate the closure above itself.
                </div>
              </>
            )}

            {rejection && (
              <>
                <div className={styles.formError}>
                  {rejection.code}
                  {rejection.message ? `: ${rejection.message}` : ""}
                </div>
                {rejection.unresolved.length > 0 && (
                  <>
                    <div className={styles.profileTableWrap}>
                      <table className={styles.table}>
                        <thead>
                          <tr>
                            <th>Dependency</th><th>Minimum</th><th>Reason</th>
                            <th>In store</th><th>Declared</th><th>Upstream</th>
                          </tr>
                        </thead>
                        <tbody>
                          {rejection.unresolved.map((u) => (
                            <tr key={u.name}>
                              <td>{u.name}</td>
                              <td>{u.min}</td>
                              <td><span className={`${styles.tableBadge} ${styles.eol}`}>{u.reason}</span></td>
                              <td>{u.foundInStore || "—"}</td>
                              <td>{u.foundDeclared || "—"}</td>
                              <td>{u.foundUpstream || "—"}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                    {rejection.unresolved
                      .filter((u) => u.remediation)
                      .map((u) => (
                        <div key={u.name} className={styles.uploadRemediation}>
                          {u.name}: {u.remediation}
                        </div>
                      ))}
                  </>
                )}
              </>
            )}
          </div>
        </>
      )}

      {/* Gaps table */}
      {status.gaps.length > 0 && (
        <>
          <h3 className={styles.sectionTitle}>Plugin gaps</h3>
          <div className={styles.profileTableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Plugin</th><th>Version</th><th>Required by</th>
                </tr>
              </thead>
              <tbody>
                {status.gaps.map((g, i) => (
                  <tr key={i}>
                    <td><span className={`${styles.tableBadge} ${styles.eol}`}>{g.plugin}</span></td>
                    <td>{g.version}</td>
                    <td>{g.requiredBy}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}

      {/* Inventory table */}
      <h3 className={styles.sectionTitle}>Plugin inventory</h3>
      <div className={styles.toolbar}>
        <input
          type="text"
          placeholder="Search plugins..."
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className={styles.formInput}
          style={{ maxWidth: 300 }}
        />
      </div>
      <div className={styles.profileTableWrap}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Name</th><th>Version</th><th>SHA256</th><th>Size</th>
            </tr>
          </thead>
          <tbody>
            {(!plugins || plugins.plugins.length === 0) ? (
              <tr>
                <td colSpan={4} className={styles.noResults}>No plugins found</td>
              </tr>
            ) : (
              plugins.plugins.map((p) => (
                <tr key={p.name}>
                  <td>{p.name}</td>
                  <td>{p.version}</td>
                  <td><code className={styles.profileCode}>{p.sha256.slice(0, 12)}…</code></td>
                  <td>{formatBytes(p.sizeBytes)}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

/** The rejection shape the panel renders, flattened out of an ApiError. */
interface UploadRejectionView {
  code: string;
  message: string;
  unresolved: UpdateCenterUnresolvedDependency[];
}

/**
 * toRejectionView pulls the update center's envelope out of an ApiError. The BFF
 * relays that body byte for byte, so the per-dependency diff is available here
 * exactly as the update center produced it.
 */
function toRejectionView(e: unknown): UploadRejectionView {
  if (e instanceof ApiError && e.body && typeof e.body === "object") {
    const body = e.body as { error?: string; message?: string; unresolved?: UpdateCenterUnresolvedDependency[] };
    return {
      code: body.error || `HTTP ${e.status}`,
      message: body.message || "",
      unresolved: body.unresolved || [],
    };
  }
  return { code: "upload failed", message: e instanceof Error ? e.message : String(e), unresolved: [] };
}
