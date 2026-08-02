import { useState, useEffect, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { listRoles, deleteRole } from "../api/client";
import type { VarroaRole } from "../types";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import styles from "./Roles.module.css";

const FILTERS = ["All", "Built-in", "Custom"] as const;

export default function Roles() {
  const navigate = useNavigate();
  const { permissions } = useAuth();
  const [roles, setRoles] = useState<VarroaRole[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [filter, setFilter] = useState<string>("All");
  const [search, setSearch] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    listRoles()
      .then((data) => setRoles(data.items || []))
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  const counts: Record<string, number> = {};
  for (const r of roles) {
    const kind = r.metadata.labels?.["varroa.dev/builtin"] === "true" ? "Built-in" : "Custom";
    counts[kind] = (counts[kind] ?? 0) + 1;
  }
  counts["All"] = roles.length;

  const filtered = useMemo(() => {
    return roles.filter((r) => {
      if (filter === "Built-in" && r.metadata.labels?.["varroa.dev/builtin"] !== "true") return false;
      if (filter === "Custom" && r.metadata.labels?.["varroa.dev/builtin"] === "true") return false;
      if (search && !r.metadata.name.toLowerCase().includes(search.toLowerCase())) return false;
      return true;
    });
  }, [roles, filter, search]);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteRole(deleteTarget);
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
          <div className={styles.pageTitle}>Roles</div>
          <div className={styles.pageDesc}>
            Each role is a <span className={styles.mono}>varroa.dev/v1alpha1 · VarroaRole</span> resource
          </div>
        </div>
        {canDoGlobal(permissions, "roles", "create") && (
          <Button variant="primary" onClick={() => navigate("/access/roles/create")}>＋ New Role</Button>
        )}
      </div>

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
        <div className={styles.loadingBanner}>Loading roles...</div>
      )}
      {error && (
        <div className={styles.errorBanner}>Failed to load: {error}</div>
      )}
      {actionError && (
        <div className={styles.errorBanner}>{actionError}</div>
      )}

      {!loading && !error && (
        <Card>
          <div className={`${styles.roleRow} ${styles.head}`}>
            <span>Name</span>
            <span>Built-in</span>
            <span>API Rules</span>
            <span>Jenkins Perms</span>
            <span />
          </div>
          {filtered.map((role) => (
            <div key={role.metadata.name} className={styles.roleRow}>
              <span className={styles.roleName}>{role.metadata.name}</span>
              <span className={styles.monoMuted}>
                {role.metadata.labels?.["varroa.dev/builtin"] === "true" ? "Yes" : "No"}
              </span>
              <span className={styles.monoMuted}>{role.spec.apiRules?.length ?? 0}</span>
              <span className={styles.monoMuted}>{role.spec.jenkinsPermissions?.length ?? 0}</span>
              <div className={styles.actions}>
                {canDoGlobal(permissions, "roles", "update") && (
                  <Button size="sm" variant="ghost" onClick={() => navigate(`/access/roles/${role.metadata.name}/edit`)}>
                    Edit
                  </Button>
                )}
                {canDoGlobal(permissions, "roles", "delete") && (
                  <Button size="sm" variant="ghost" onClick={() => setDeleteTarget(role.metadata.name)}>
                    Delete
                  </Button>
                )}
              </div>
            </div>
          ))}
          {filtered.length === 0 && (
            <div className={styles.empty}>
              {roles.length === 0
                ? "No roles found."
                : "No roles match the current filters."}
            </div>
          )}
        </Card>
      )}

      {deleteTarget && (
        <div className={styles.overlay} onClick={() => setDeleteTarget(null)}>
          <div className={styles.dialog} onClick={(e) => e.stopPropagation()}>
            <div className={styles.dialogTitle}>Delete Role</div>
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
