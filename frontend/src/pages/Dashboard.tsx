import { Link } from "react-router-dom";
import { useControllers, useControllerFanoutFailures } from "../hooks/useControllers";
import { useClusters } from "../hooks/useClusters";
import { age } from "../components/activityTimeline.util";
import type { ClusterEntry } from "../types";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere, canSeePipelines } from "../hooks/usePermissions";
import ErrorBoundary from "../components/ErrorBoundary";
import { RecoveryState } from "../components/RecoveryState";
import { usePipelinesOverview } from "./pipelines/usePipelinesOverview";
import { ControllersSection } from "./overview/ControllersSection";
import { PipelinesSection } from "./overview/PipelinesSection";
import { controllerCounts, overviewSubtitle } from "./overview/overviewText";
import styles from "./Dashboard.module.css";
import { ConnectClientTile } from "./ConnectClientTile";

/**
 * Dashboard is the Overview page. It is a shell: the Pipelines section and the
 * Controllers section each show when the caller can read it. Each section sits in
 * its own error boundary, so a failure in one never hides the other.
 */
export default function Dashboard() {
  const { permissions, permissionsLoading, permissionsError, retryPermissions } = useAuth();
  const canPipelines = canSeePipelines(permissions);
  const canControllers = canDoAnywhere(permissions, "controllers", "read");
  const overview = usePipelinesOverview(canPipelines);
  const controllers = useControllers();
  const clusters = useClusters();

  const permsFailed = permissionsError !== null && permissions === undefined;
  const tenantCount = canPipelines && overview.isSuccess ? overview.data.tenants.length : undefined;
  const controllerPart = canControllers && controllers.data ? controllerCounts(controllers.data) : undefined;
  let subtitle = overviewSubtitle({ tenants: tenantCount, controllers: controllerPart });
  const reading = permissionsLoading || (canPipelines && overview.isPending) || (canControllers && controllers.isPending);
  if (subtitle === "" && reading) subtitle = "Loading...";

  let body;
  if (permissionsLoading) {
    body = null;
  } else if (permsFailed) {
    body = (
      <RecoveryState
        compact
        kind="error"
        title="Unable to load your access"
        message="Varroa could not read your permissions."
        onRetry={retryPermissions}
      />
    );
  } else if (!canPipelines && !canControllers) {
    body = (
      <RecoveryState
        compact
        kind="403"
        title="No access"
        message="No role gives you access to pipelines or controllers. Ask an administrator to add you."
      />
    );
  } else {
    body = (
      <>
        {canPipelines && (
          <ErrorBoundary>
            <PipelinesSection />
          </ErrorBoundary>
        )}
        {canControllers && (
          <ErrorBoundary>
            <ControllersSection />
          </ErrorBoundary>
        )}
      </>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <h1 className={styles.pageTitle}>Overview</h1>
          {subtitle !== "" && <div className={styles.pageDesc}>{subtitle}</div>}
        </div>
      </div>
      <ClusterStrip clusters={clusters.data} isLoading={clusters.isLoading} error={!!clusters.error} />
      {body}
      {!permissionsLoading && !permsFailed && <ConnectClientTile />}
    </div>
  );
}

function ClusterStrip({ clusters, isLoading, error }: { clusters: ClusterEntry[] | undefined; isLoading: boolean; error: boolean }) {
  const failedNames = new Set((useControllerFanoutFailures().data ?? []).map((f) => f.name));
  if (isLoading || error || (clusters?.length ?? 0) < 2) return null;
  const cls = [...(clusters ?? [])].sort((a, b) => (a.core ? -1 : b.core ? 1 : a.name.localeCompare(b.name)));
  return (
    <div className={styles.clusterStrip}>
      {cls.map((c) => {
        const listFailed = failedNames.has(c.name);
        return (
        <Link key={c.name} to={`/controllers?cluster=${encodeURIComponent(c.name)}`} className={styles.clusterCard}>
          <div className={styles.clusterCardHead}>
            <span className={styles.clusterDot} data-health={c.healthy && !listFailed ? "ok" : "bad"} />
            <span className={styles.clusterName}>{c.name}</span>
            {c.core && <span className={styles.coreTag}>core</span>}
          </div>
          <div className={styles.clusterCardStat}>
            {listFailed ? "list unavailable" : `${c.connectedCount}/${c.controllerCount} controllers`}
          </div>
          <div className={styles.clusterCardAge}>{age(c.lastHeartbeat, { variant: "heartbeat" })}</div>
        </Link>
        );
      })}
    </div>
  );
}

