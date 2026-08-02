import { useState, useEffect, useMemo } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { listJenkinsRoleBindings, deleteJenkinsRoleBinding } from "../api/client";
import type { JenkinsRoleBinding } from "../types";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import ClusterSelector from "../components/ClusterSelector";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import { clusterQuery } from "../routing";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import styles from "./JenkinsRoleBindings.module.css";
import tableStyles from "./JenkinsRBACTable.module.css";

function formatJenkinsScope(binding: JenkinsRoleBinding): string {
  const js = binding.spec.jenkinsScope;
  if (!js) return "—";
  if (js.type === "Folder" && js.folder) return `Folder: ${js.folder}`;
  if (js.type === "Pattern" && js.pattern) return `Pattern: ${js.pattern}`;
  return js.type || "—";
}

export default function JenkinsRoleBindings() {
  const navigate = useNavigate();
  const { permissions } = useAuth();
  const [bindings, setBindings] = useState<JenkinsRoleBinding[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [search, setSearch] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [searchParams, setSearchParams] = useSearchParams();
  const { cluster, ready } = useConfigurationCluster();

  useEffect(() => {
    if (!cluster) return;
    setLoading(true);
    setError("");
    listJenkinsRoleBindings(cluster)
      .then((data) => setBindings(data.items || []))
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, [cluster]);

  const filtered = useMemo(() => {
    return bindings.filter((b) => {
      if (search && !b.metadata.name.toLowerCase().includes(search.toLowerCase())) return false;
      return true;
    });
  }, [bindings, search]);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteJenkinsRoleBinding(cluster!, deleteTarget);
      setBindings((prev) => prev.filter((b) => b.metadata.name !== deleteTarget));
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
          <div className={styles.pageTitle}>Jenkins Role Bindings</div>
          <div className={styles.pageDesc}>
            Bind subjects to pure Jenkins roles —{" "}
            <span className={styles.mono}>varroa.dev/v1alpha1 · JenkinsRoleBinding</span>
          </div>
        </div>
        <div style={{ display: "flex", alignItems: "flex-end", gap: 12 }}>
          {cluster && <ClusterSelector value={cluster} onChange={(value) => {
            const next = new URLSearchParams(searchParams);
            next.set("cluster", value);
            setSearchParams(next);
          }} />}
          {canDoGlobal(permissions, "jenkinsrolebindings", "create") && (
            <Button variant="primary" onClick={() => navigate(`/access/jenkins-role-bindings/create${clusterQuery(cluster)}`)}>
              ＋ New Jenkins Binding
            </Button>
          )}
        </div>
      </div>

      {ready && !cluster && <NoAccessibleClusters />}
      <div className={styles.filterBar}>
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
        <div className={styles.loadingBanner}>Loading Jenkins role bindings...</div>
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
            <thead><tr><th>Name</th><th>Cluster</th><th>Role</th><th>Subjects</th><th>Scope</th><th>Actions</th></tr></thead>
            <tbody>{filtered.map((b) => (
            <tr key={b.metadata.name}>
              <td data-label="Name" className={tableStyles.name}>{b.metadata.name}</td>
              <td data-label="Cluster" className={tableStyles.mono}>{cluster}</td>
              <td data-label="Role" className={tableStyles.mono}>{b.spec.roleRef}</td>
              <td data-label="Subjects">{(b.spec.subjects || []).map((subject) => <span className={tableStyles.badge} key={`${subject.kind}:${subject.name}`}>{subject.kind}: {subject.name}</span>)}</td>
              <td data-label="Scope"><span className={tableStyles.scopeBadge}>{formatJenkinsScope(b)}</span></td>
              <td data-label="Actions"><div className={tableStyles.actions}>
                {canDoGlobal(permissions, "jenkinsrolebindings", "update") && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() =>
                      navigate(`/access/jenkins-role-bindings/${b.metadata.name}/edit${clusterQuery(cluster)}`)
                    }
                  >
                    Edit
                  </Button>
                )}
                {canDoGlobal(permissions, "jenkinsrolebindings", "delete") && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setDeleteTarget(b.metadata.name)}
                  >
                    Delete
                  </Button>
                )}
              </div></td>
            </tr>
          ))}</tbody>
          </table>
          </div>
          {filtered.length === 0 && (
            <div className={styles.empty}>
              {bindings.length === 0
                ? "No Jenkins role bindings found."
                : "No bindings match the current filter."}
            </div>
          )}
        </Card>
      )}

      {deleteTarget && (
        <div className={styles.overlay} onClick={() => setDeleteTarget(null)}>
          <div className={styles.dialog} onClick={(e) => e.stopPropagation()}>
            <div className={styles.dialogTitle}>Delete Jenkins Role Binding</div>
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
