import { useState, useEffect, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { listRoleBindings, deleteRoleBinding } from "../api/client";
import type { VarroaRoleBinding } from "../types";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import styles from "./RoleBindings.module.css";

const SCOPE_FILTERS = ["All", "Cluster-wide", "Scoped"] as const;

function formatSubjects(binding: VarroaRoleBinding): string {
  return (binding.spec.subjects || [])
    .map((s) => `${s.kind}:${s.name}`)
    .join(", ");
}

function formatScope(binding: VarroaRoleBinding): string {
  const scope = binding.spec.scope;
  if (!scope) return "Cluster-wide";
  const parts: string[] = [];
  if (scope.namespaces && scope.namespaces.length > 0) {
    parts.push(`ns:${scope.namespaces.join(",")}`);
  }
  if (scope.controllerSelector?.matchLabels) {
    const labels = Object.entries(scope.controllerSelector.matchLabels)
      .filter(([, v]) => v !== undefined && v !== "")
      .map(([k, v]) => `${k}=${v}`)
      .join(",");
    if (labels) parts.push(`sel:${labels}`);
  }
  return parts.length > 0 ? parts.join(" ") : "Cluster-wide";
}

function scopeCategory(binding: VarroaRoleBinding): string {
  if (!binding.spec.scope) return "Cluster-wide";
  return "Scoped";
}

export default function RoleBindings() {
  const navigate = useNavigate();
  const { permissions } = useAuth();
  const [bindings, setBindings] = useState<VarroaRoleBinding[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [filter, setFilter] = useState<string>("All");
  const [search, setSearch] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    listRoleBindings()
      .then((data) => setBindings(data.items || []))
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  const counts: Record<string, number> = {};
  for (const b of bindings) {
    const cat = scopeCategory(b);
    counts[cat] = (counts[cat] ?? 0) + 1;
  }
  counts["All"] = bindings.length;

  const filtered = useMemo(() => {
    return bindings.filter((b) => {
      if (filter !== "All" && scopeCategory(b) !== filter) return false;
      if (search && !b.metadata.name.toLowerCase().includes(search.toLowerCase())) return false;
      return true;
    });
  }, [bindings, filter, search]);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteRoleBinding(deleteTarget);
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
          <div className={styles.pageTitle}>Role Bindings</div>
          <div className={styles.pageDesc}>
            Each binding is a <span className={styles.mono}>varroa.dev/v1alpha1 · VarroaRoleBinding</span> resource
          </div>
        </div>
        {canDoGlobal(permissions, "rolebindings", "create") && (
          <Button variant="primary" onClick={() => navigate("/access/role-bindings/create")}>＋ New Role Binding</Button>
        )}
      </div>

      <div className={styles.filterBar}>
        <div className={styles.chips}>
          {SCOPE_FILTERS.map((f) => (
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
        <div className={styles.loadingBanner}>Loading role bindings...</div>
      )}
      {error && (
        <div className={styles.errorBanner}>Failed to load: {error}</div>
      )}
      {actionError && (
        <div className={styles.errorBanner}>{actionError}</div>
      )}

      {!loading && !error && (
        <Card>
          <div className={`${styles.bindingRow} ${styles.head}`}>
            <span>Name</span>
            <span>Subjects</span>
            <span>Role Ref</span>
            <span>Scope</span>
            <span />
          </div>
          {filtered.map((binding) => (
            <div key={binding.metadata.name} className={styles.bindingRow}>
              <span className={styles.bindingName}>{binding.metadata.name}</span>
              <span className={styles.cellMuted}>{formatSubjects(binding)}</span>
              <span>{binding.spec.roleRef}</span>
              <span className={styles.cellMuted}>{formatScope(binding)}</span>
              <div className={styles.actions}>
                {canDoGlobal(permissions, "rolebindings", "update") && (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => navigate(`/access/role-bindings/${binding.metadata.name}/edit`)}
                  >
                    Edit
                  </Button>
                )}
                {canDoGlobal(permissions, "rolebindings", "delete") && (
                  <Button size="sm" variant="ghost" onClick={() => setDeleteTarget(binding.metadata.name)}>
                    Delete
                  </Button>
                )}
              </div>
            </div>
          ))}
          {filtered.length === 0 && (
            <div className={styles.empty}>
              {bindings.length === 0
                ? "No role bindings found."
                : "No role bindings match the current filters."}
            </div>
          )}
        </Card>
      )}

      {deleteTarget && (
        <div className={styles.overlay} onClick={() => setDeleteTarget(null)}>
          <div className={styles.dialog} onClick={(e) => e.stopPropagation()}>
            <div className={styles.dialogTitle}>Delete Role Binding</div>
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
