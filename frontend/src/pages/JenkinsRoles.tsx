import { useState, useEffect, useMemo } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { listJenkinsRoles, deleteJenkinsRole } from "../api/client";
import type { JenkinsRole } from "../types";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import ClusterSelector from "../components/ClusterSelector";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import { clusterQuery } from "../routing";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import styles from "./JenkinsRoles.module.css";
import tableStyles from "./JenkinsRBACTable.module.css";

const FILTERS = ["All", "Global", "Item", "Agent"] as const;

export default function JenkinsRoles() {
  const navigate = useNavigate();
  const { permissions } = useAuth();
  const [roles, setRoles] = useState<JenkinsRole[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [filter, setFilter] = useState<string>("All");
  const [search, setSearch] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [searchParams, setSearchParams] = useSearchParams();
  const { cluster, ready } = useConfigurationCluster();

  useEffect(() => {
    if (!cluster) return;
    setLoading(true);
    setError("");
    listJenkinsRoles(cluster)
      .then((data) => setRoles(data.items || []))
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, [cluster]);

  const counts: Record<string, number> = {};
  for (const r of roles) {
    const kind = r.spec.roleType || "Unknown";
    counts[kind] = (counts[kind] ?? 0) + 1;
  }
  counts["All"] = roles.length;

  const filtered = useMemo(() => {
    return roles.filter((r) => {
      if (filter !== "All" && r.spec.roleType !== filter) return false;
      if (search && !r.metadata.name.toLowerCase().includes(search.toLowerCase())) return false;
      return true;
    });
  }, [roles, filter, search]);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteJenkinsRole(cluster!, deleteTarget);
      setRoles((prev) => prev.filter((r) => r.metadata.name !== deleteTarget));
      setDeleteTarget(null);
      setActionError("");
    } catch (err: any) {
      setActionError(err.message);
    }
    setDeleting(false);
  };

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Jenkins Roles</div>
          <div className={styles.pageDesc}>
            Pure Jenkins permission sets —{" "}
            <span className={styles.mono}>varroa.dev/v1alpha1 · JenkinsRole</span>
          </div>
        </div>
        <div style={{ display: "flex", alignItems: "flex-end", gap: 12 }}>
          {cluster && <ClusterSelector value={cluster} onChange={(value) => {
            const next = new URLSearchParams(searchParams);
            next.set("cluster", value);
            setSearchParams(next);
          }} />}
          {canDoGlobal(permissions, "jenkinsroles", "create") && (
            <Button variant="primary" onClick={() => navigate(`/access/jenkins-roles/create${clusterQuery(cluster)}`)}>
              ＋ New Jenkins Role
            </Button>
          )}
        </div>
      </div>

      {ready && !cluster && <NoAccessibleClusters />}
      <div className={styles.filterBar}>
        <div className={styles.chips}>
          {FILTERS.map((f) => (
            <button
              key={f}
              className={`${styles.chip} ${filter === f ? styles.chipOn : ""}`}
              onClick={() => setFilter(f)}
            >
              {f} <span className={styles.chipCount}>{counts[f] ?? 0}</span>
            </button>
          ))}
        </div>
        <div className={styles.searchBox}>
          <span>⌕</span>
          <input
            placeholder="Filter by name..."
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </div>
      </div>

      {loading && (
        <div className={styles.loadingBanner}>Loading Jenkins roles...</div>
      )}
      {error && (
        <div className={styles.errorBanner}>Failed to load: {error}</div>
      )}
      {actionError && (
        <div className={styles.errorBanner}>{actionError}</div>
      )}

      {!loading && !error && (
        <Card>
          <div className={tableStyles.scroll}>
          <table className={tableStyles.table}>
            <thead><tr><th>Name</th><th>Cluster</th><th>Global</th><th>Item</th><th>Agent</th><th>Actions</th></tr></thead>
            <tbody>{filtered.map((role) => {
              const permissionList = role.spec.permissions?.join(", ") || "—";
              return <tr key={role.metadata.name}>
              <td data-label="Name" className={tableStyles.name}>{role.metadata.name}</td>
              <td data-label="Cluster" className={tableStyles.mono}>{cluster}</td>
              <td data-label="Global" className={tableStyles.mono}>{role.spec.roleType === "Global" ? permissionList : "—"}</td>
              <td data-label="Item" className={tableStyles.mono}>{role.spec.roleType === "Item" ? permissionList : "—"}</td>
              <td data-label="Agent" className={tableStyles.mono}>{role.spec.roleType === "Agent" ? permissionList : "—"}</td>
              <td data-label="Actions"><div className={tableStyles.actions}>
                {canDoGlobal(permissions, "jenkinsroles", "update") && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => navigate(`/access/jenkins-roles/${role.metadata.name}/edit${clusterQuery(cluster)}`)}
                  >
                    Edit
                  </Button>
                )}
                {canDoGlobal(permissions, "jenkinsroles", "delete") && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setDeleteTarget(role.metadata.name)}
                  >
                    Delete
                  </Button>
                )}
              </div></td>
            </tr>})}</tbody>
          </table>
          </div>
          {filtered.length === 0 && (
            <div className={styles.empty}>
              {roles.length === 0
                ? "No Jenkins roles found."
                : "No Jenkins roles match the current filters."}
            </div>
          )}
        </Card>
      )}

      {deleteTarget && (
        <div className={styles.overlay} onClick={() => setDeleteTarget(null)}>
          <div className={styles.dialog} onClick={(e) => e.stopPropagation()}>
            <div className={styles.dialogTitle}>Delete Jenkins Role</div>
            <p className={styles.dialogBody}>
              Are you sure you want to delete <b>{deleteTarget}</b>?
              This action cannot be undone.
            </p>
            <div className={styles.dialogActions}>
              <Button onClick={() => setDeleteTarget(null)}>Cancel</Button>
              <Button variant="primary" onClick={handleDelete} disabled={deleting}>
                {deleting ? "Deleting..." : "Delete"}
              </Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
