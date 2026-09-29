import { Link, Navigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Waypoints } from "lucide-react";
import { listPipelineTeams } from "../../api/client";
import { usePermissions } from "../../hooks/usePermissions";
import { RecoveryState } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import styles from "./PipelinesIndex.module.css";

/**
 * PipelinesIndex is the /pipelines landing page. It lists the Teams the
 * caller may open (design ruling aa) and redirects when exactly one
 * qualifies. A Team qualifies through a GET /pipelines row, which covers
 * every pipelines Team of this cluster and of each member cluster that the
 * caller may read. The caller's `tenants` grants also add names: controller
 * tenants and Teams without spec.pipelines, which GET /pipelines does not
 * list. When the GET /pipelines read fails, the page lists the grants only.
 */
export default function PipelinesIndex() {
  const { data: perms, isLoading: permsLoading, error: permsError } = usePermissions();

  const {
    data: pipelineTeams,
    isLoading: teamsLoading,
    isError: teamsErrored,
  } = useQuery({
    queryKey: ["pipelineTeams"],
    queryFn: listPipelineTeams,
  });

  if (permsError) {
    return <RecoveryState kind="error" title="Unable to load page" message="Varroa could not load this page." />;
  }

  if (permsLoading || teamsLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading teams…</div>
      </div>
    );
  }

  // One name that two member clusters hold answers two rows. Both rows
  // open the same URL, so the page shows the name once.
  const names = new Set<string>();
  for (const t of perms?.tenants ?? []) names.add(t.tenant);
  if (!teamsErrored) {
    for (const t of pipelineTeams?.items ?? []) names.add(t.name);
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
