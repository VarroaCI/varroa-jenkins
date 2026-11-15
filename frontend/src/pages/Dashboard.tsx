import { Link } from "react-router-dom";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere, canDoGlobal, canSeePipelines } from "../hooks/usePermissions";
import ErrorBoundary from "../components/ErrorBoundary";
import { RecoveryState } from "../components/RecoveryState";
import { ClusterFanoutNotice } from "../components/ClusterFanoutNotice";
import { usePipelinesOverview } from "./pipelines/usePipelinesOverview";
import { AttentionPanel } from "./overview/AttentionPanel";
import { DayPanel } from "./overview/DayPanel";
import { TodayFeed } from "./overview/TodayFeed";
import { useOverviewLive } from "./overview/useOverviewLive";
import styles from "./Dashboard.module.css";
import { ConnectClientTile } from "./ConnectClientTile";

/**
 * Dashboard is the Overview page. It reads top to bottom: the items that need a
 * person, the last 24 hours beside the day's feed, and the connect tile. Each part shows only what the
 * caller can read, and sits in its own error boundary, so a failure in one part
 * never hides another.
 */
export default function Dashboard() {
  const { permissions, permissionsLoading, permissionsError, retryPermissions } = useAuth();
  const canPipelines = canSeePipelines(permissions);
  const canControllers = canDoAnywhere(permissions, "controllers", "read");
  const canCreateController = canDoAnywhere(permissions, "controllers", "create");
  const isAdmin = canDoGlobal(permissions, "*", "*");
  const overview = usePipelinesOverview(canPipelines);
  useOverviewLive(canPipelines && overview.isSuccess && overview.data.tenants.length > 0);

  const permsFailed = permissionsError !== null && permissions === undefined;

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
        <ErrorBoundary>
          <AttentionPanel canPipelines={canPipelines} canControllers={canControllers} isAdmin={isAdmin} />
        </ErrorBoundary>
        <div className={styles.day}>
          <ErrorBoundary>
            <DayPanel canPipelines={canPipelines} canControllers={canControllers} />
          </ErrorBoundary>
          <ErrorBoundary>
            <TodayFeed canPipelines={canPipelines} />
          </ErrorBoundary>
        </div>
      </>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <h1 className={styles.pageTitle}>Overview</h1>
        {canCreateController && (
          <div className={styles.actions}>
            <Link to="/controllers/create" className={styles.btnPrimary}>
              New controller
            </Link>
          </div>
        )}
      </div>
      {canControllers && <ClusterFanoutNotice />}
      {body}
      {!permissionsLoading && !permsFailed && <ConnectClientTile />}
    </div>
  );
}
