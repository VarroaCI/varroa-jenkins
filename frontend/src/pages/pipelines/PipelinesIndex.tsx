import { Link, Navigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { ChevronRight, Waypoints } from "lucide-react";
import { listPipelineTenants } from "../../api/client";
import { RecoveryState } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { indexTitle } from "./pageTitles";
import styles from "./PipelinesIndex.module.css";

/**
 * PipelinesIndex is the /pipelines landing page. It lists the Tenants the
 * caller may open (design ruling aa) and redirects when exactly one
 * qualifies. Every name comes from a GET /pipelines row. That read covers
 * the pipelines Tenants of this cluster and of each member cluster, and the
 * local Tenants whose namespaces hold a controller the caller may read. A
 * name in the caller's `tenants` grants that GET /pipelines does not
 * answer is not listed. When the GET /pipelines read fails, the page shows
 * an error with Retry and no link.
 */
export default function PipelinesIndex() {
  const {
    data: pipelineTenants,
    isLoading: tenantsLoading,
    isError: tenantsErrored,
    refetch: refetchTenants,
  } = useQuery({
    queryKey: ["pipelineTenants"],
    queryFn: listPipelineTenants,
  });

  useDocumentTitle(indexTitle());

  if (tenantsLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading tenants…</div>
      </div>
    );
  }

  if (tenantsErrored) {
    return (
      <RecoveryState
        kind="error"
        title="Unable to load tenants"
        message="Varroa could not load the tenant list."
        onRetry={() => void refetchTenants()}
      />
    );
  }

  // One name that two member clusters hold answers two rows. Both rows
  // open the same URL, so the page shows the name once.
  const names = new Set<string>();
  for (const t of pipelineTenants?.items ?? []) names.add(t.name);
  const tenants = Array.from(names).sort((a, b) => a.localeCompare(b));

  if (tenants.length === 0) {
    return (
      <RecoveryState
        kind="403"
        title="No pipelines access"
        message="No Tenant grants you pipelines access. Ask a Tenant admin to add you."
      />
    );
  }

  if (tenants.length === 1) {
    return <Navigate to={`/pipelines/${encodeURIComponent(tenants[0])}`} replace />;
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div className={styles.pageTitle}>Pipelines</div>
        <div className={styles.pageDesc}>Pick a Tenant to see its pipelines.</div>
      </div>
      <Card>
        <ul className={styles.list}>
          {tenants.map((name) => (
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
