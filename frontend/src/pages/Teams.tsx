import { useState, useEffect, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { listTeams, deleteTeam } from "../api/client";
import type { TeamEntry } from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import styles from "./Teams.module.css";

export default function Teams() {
  const navigate = useNavigate();
  const { permissions } = useAuth();
  const [teams, setTeams] = useState<TeamEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [search, setSearch] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  useEffect(() => {
    listTeams()
      .then((data) => setTeams(data || []))
      .catch((err) => setError(err.message))
      .finally(() => setLoading(false));
  }, []);

  const filtered = useMemo(() => {
    return teams.filter((t) => {
      if (search && !t.name.toLowerCase().includes(search.toLowerCase())) return false;
      return true;
    });
  }, [teams, search]);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setDeleting(true);
    try {
      await deleteTeam(deleteTarget);
      setTeams((prev) => prev.filter((t) => t.name !== deleteTarget));
      setDeleteTarget(null);
    } catch (err: unknown) {
      setActionError(err instanceof Error ? err.message : "delete failed");
    } finally {
      setDeleting(false);
    }
  };

  const isAdmin = canDoGlobal(permissions, "*", "*");

  return (
    <div className={styles.teamsPage}>
      <div className={styles.header}>
        <h1>Teams</h1>
        {isAdmin && (
          <Button onClick={() => navigate("/access/teams/create")}>Create Team</Button>
        )}
      </div>

      {actionError && <div className={styles.errors}>{actionError}</div>}
      {error && <div className={styles.errors}>{error}</div>}

      <div className={styles.toolbar}>
        <input
          className={styles.searchInput}
          type="text"
          placeholder="Search teams..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </div>

      {loading ? (
        <p>Loading...</p>
      ) : filtered.length === 0 ? (
        <p>No teams found.</p>
      ) : (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Name</th>
                <th>Namespaces</th>
                <th>Role</th>
                <th>Status</th>
                {isAdmin && <th>Actions</th>}
              </tr>
            </thead>
            <tbody>
              {filtered.map((team) => {
                const readyCond = team.conditions?.find((c) => c.type === "TeamReady");
                const isReady = readyCond?.status === "True";
                return (
                  <tr key={team.name}>
                    <td className={styles.nameCell}>
                      {isAdmin ? (
                        <a
                          href="#"
                          onClick={(e) => {
                            e.preventDefault();
                            navigate(`/access/teams/${team.name}/edit`);
                          }}
                        >
                          {team.name}
                        </a>
                      ) : (
                        team.name
                      )}
                    </td>
                    <td>{(team.namespaces || []).join(", ")}</td>
                    <td>{team.roleRef || "developer"}</td>
                    <td>
                      <span
                        className={`${styles.statusBadge} ${
                          isReady ? styles.statusReady : styles.statusNotReady
                        }`}
                      >
                        {isReady ? "Ready" : readyCond?.reason || "Unknown"}
                      </span>
                    </td>
                    {isAdmin && (
                      <td>
                        <button
                          className={`${styles.actionBtn} ${styles.deleteBtn}`}
                          onClick={() => setDeleteTarget(team.name)}
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
        </Card>
      )}

      {deleteTarget && (
        <div className="modal-overlay">
          <div className="modal">
            <p>Delete team "{deleteTarget}"?</p>
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
