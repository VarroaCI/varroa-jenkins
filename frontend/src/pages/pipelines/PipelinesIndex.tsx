import { Link, Navigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Waypoints } from "lucide-react";
import { listPipelineTeams } from "../../api/client";
import { RecoveryState } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import styles from "./PipelinesIndex.module.css";

/**
 * PipelinesIndex is the /pipelines landing page. It lists the Teams the
 * caller may open (design ruling aa) and redirects when exactly one
 * qualifies. Every name comes from a GET /pipelines row. That read covers
 * the pipelines Teams of this cluster and of each member cluster, and the
 * local Teams whose namespaces hold a controller the caller may read. A
 * name in the caller's `tenants` grants that GET /pipelines does not
 * answer is not listed. When the GET /pipelines read fails, the page shows
 * an error with Retry and no link.
 */
export default function PipelinesIndex() {
  const {
    data: pipelineTeams,
    isLoading: teamsLoading,
    isError: teamsErrored,
    refetch: refetchTeams,
  } = useQuery({
    queryKey: ["pipelineTeams"],
    queryFn: listPipelineTeams,
  });

  if (teamsLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading teams…</div>
      </div>
    );
  }

  if (teamsErrored) {
    return (
      <RecoveryState
        kind="error"
        title="Unable to load teams"
        message="Varroa could not load the team list."
        onRetry={() => void refetchTeams()}
      />
    );
  }

  // One name that two member clusters hold answers two rows. Both rows
  // open the same URL, so the page shows the name once.
  const names = new Set<string>();
  for (const t of pipelineTeams?.items ?? []) names.add(t.name);
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
