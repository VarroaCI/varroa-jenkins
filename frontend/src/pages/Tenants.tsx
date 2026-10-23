import { useState, useEffect, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { listTenants, deleteTenant } from "../api/client";
import type { TenantEntry } from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import { tenantMemberLabel } from "../types";
import styles from "./Tenants.module.css";

/**
 * exportSummaryRows renders one line per spec.pipelines.exports entry, its
 * kind (job or locks), and its callers, so the list shows every grant a
 * tenant makes without hiding which kind each entry is.
 */
function exportSummaryRows(tenant: TenantEntry) {
  const exports = tenant.pipelines?.exports;
  if (!exports || exports.length === 0) return "—";
  return (
    <ul className={styles.exportsList}>
      {exports.map((exp, i) => (
        <li key={i}>
          <span className={styles.exportKind}>{exp.job ? "job" : "locks"}:</span>{" "}
          {exp.job || (exp.locks ?? []).join(", ")} → {exp.callers.join(", ")}
        </li>
      ))}
    </ul>
  );
}

export default function Tenants() {
  const navigate = useNavigate();
  const { permissions } = useAuth();
  const [tenants, setTenants] = useState<TenantEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [search, setSearch] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    listTenants()
      .then((data) => setTenants(data || []))
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  const filtered = useMemo(() => {
    return tenants.filter((t) => {
      if (search && !t.name.toLowerCase().includes(search.toLowerCase())) return false;
      return true;
    });
  }, [tenants, search]);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteTenant(deleteTarget);
      setTenants((prev) => prev.filter((t) => t.name !== deleteTarget));
      setDeleteTarget(null);
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : "delete failed");
    } finally {
      setDeleting(false);
    }
  };

  const isAdmin = canDoGlobal(permissions, "*", "*");

  return (
    <div className={styles.tenantsPage}>
      <div className={styles.header}>
        <h1>Tenants</h1>
        {isAdmin && (
          <Button onClick={() => navigate("/access/tenants/create")}>Create Tenant</Button>
        )}
      </div>

      {actionError && <div className={styles.errors}>{actionError}</div>}
      {error && <div className={styles.errors}>{error}</div>}

      <div className={styles.toolbar}>
        <input
          className={styles.searchInput}
          type="text"
          placeholder="Search tenants..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </div>

      {loading ? (
        <p>Loading...</p>
      ) : filtered.length === 0 ? (
        <p>No tenants found.</p>
      ) : (
        <Card>
          <div className={styles.tableWrap}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Namespaces</th>
                  <th>Role</th>
                  <th>Status</th>
                  <th>Placement</th>
                  <th>Rehydration</th>
                  <th>Exports</th>
                  {isAdmin && <th>Actions</th>}
                </tr>
              </thead>
              <tbody>
                {filtered.map((tenant) => {
                  const readyCond = tenant.conditions?.find((c) => c.type === "TenantReady");
                  const isReady = readyCond?.status === "True";
                  const placedCond = tenant.conditions?.find((c) => c.type === "Placed");
                  const rehydratedCond = tenant.conditions?.find((c) => c.type === "Rehydrated");
                  return (
                    <tr key={tenant.name}>
                      <td className={styles.nameCell}>
                        {isAdmin ? (
                          <a
                            href="#"
                            onClick={(e) => {
                              e.preventDefault();
                              navigate(`/access/tenants/${tenant.name}/edit`);
                            }}
                          >
                            {tenant.name}
                          </a>
                        ) : (
                          tenant.name
                        )}
                      </td>
                      <td>{(tenant.namespaces || []).join(", ")}</td>
                      <td>{tenant.roleRef || "developer"}</td>
                      <td>
                        <span
                          className={`${styles.statusBadge} ${
                            isReady ? styles.statusReady : styles.statusNotReady
                          }`}
                        >
                          {isReady ? "Ready" : readyCond?.reason || "Unknown"}
                        </span>
                      </td>
                      <td className={styles.placementCell}>
                        {tenant.placement?.members && tenant.placement.members.length > 0 ? (
                          <ul className={styles.memberList}>
                            {tenant.placement.members.map((member) => (
                              <li key={`${member.cellNamespace}/${member.cell}`}>
                                {tenantMemberLabel(member)}
                              </li>
                            ))}
                          </ul>
                        ) : tenant.placement?.cell ? (
                          `${tenant.placement.cluster}/${tenant.placement.cell}`
                        ) : placedCond ? (
                          placedCond.reason || "Not placed"
                        ) : (
                          "—"
                        )}
                      </td>
                      <td>
                        {!tenant.pipelines
                          ? "—"
                          : rehydratedCond?.status === "True"
                          ? "Rehydrated"
                          : rehydratedCond?.reason || "Pending"}
                      </td>
                      <td>{exportSummaryRows(tenant)}</td>
                      {isAdmin && (
                        <td>
                          <button
                            className={`${styles.actionBtn} ${styles.deleteBtn}`}
                            onClick={() => setDeleteTarget(tenant.name)}
                          >
                            Delete
                          </button>
                        </td>
                      )}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      {deleteTarget && (
        <div className="modal-overlay">
          <div className="modal">
            <p>Delete tenant "{deleteTarget}"?</p>
            <div className={styles.formActions}>
              <Button onClick={handleDelete} disabled={deleting}>
                {deleting ? "Deleting..." : "Delete"}
              </Button>
              <Button onClick={() => setDeleteTarget(null)}>Cancel</Button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
