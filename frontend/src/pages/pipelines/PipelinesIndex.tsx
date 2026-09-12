import { Link, Navigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Waypoints } from "lucide-react";
import { listTeams } from "../../api/client";
import { usePermissions, canDoAnywhere } from "../../hooks/usePermissions";
import { RecoveryState } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import styles from "./PipelinesIndex.module.css";

/**
 * PipelinesIndex is the /pipelines landing page. It lists the Teams the
 * caller may open (design ruling aa) and redirects when exactly one
 * qualifies. A Team qualifies through the caller's `tenants` grants, or,
 * for a caller holding a global pipelines verb, through every Team the
 * `/teams` endpoint returns.
 */
export default function PipelinesIndex() {
  const { data: perms, isLoading: permsLoading, error: permsError } = usePermissions();

  const hasGlobalAccess = canDoAnywhere(perms, "pipelines", "read");
  const {
    data: allTeams,
    isLoading: teamsLoading,
    isError: teamsErrored,
  } = useQuery({
    queryKey: ["teams"],
    queryFn: listTeams,
    enabled: hasGlobalAccess,
  });

  if (permsLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading teams…</div>
      </div>
    );
  }

  if (permsError) {
    return <RecoveryState kind="error" title="Unable to load page" message="Varroa could not load this page." />;
  }

  if (hasGlobalAccess && teamsLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading teams…</div>
      </div>
    );
  }

  const names = new Set<string>();
  for (const t of perms?.tenants ?? []) names.add(t.tenant);
  if (hasGlobalAccess && !teamsErrored) {
    for (const t of allTeams ?? []) names.add(t.name);
  }
  const teams = Array.from(names).sort((a, b) => a.localeCompare(b));

  if (teams.length === 0) {
    return (
      <RecoveryState
        kind="403"
        title="No pipelines access"
        message="No Team grants you pipelines access. Ask a Team admin to add you."
      />
    );
  }

  if (teams.length === 1) {
    return <Navigate to={`/pipelines/${encodeURIComponent(teams[0])}`} replace />;
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div className={styles.pageTitle}>Pipelines</div>
        <div className={styles.pageDesc}>Pick a Team to see its pipelines.</div>
      </div>
      <Card>
        <ul className={styles.list}>
          {teams.map((name) => (
            <li key={name}>
              <Link to={`/pipelines/${encodeURIComponent(name)}`} className={styles.item}>
                <Waypoints aria-hidden="true" size={18} className={styles.itemIcon} />
                <span className={styles.itemLabel}>{name}</span>
                <ChevronRight aria-hidden="true" size={16} className={styles.itemChevron} />
              </Link>
            </li>
          ))}
        </ul>
      </Card>
    </div>
  );
}
