import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ChevronRight } from "lucide-react";
import { Card } from "../../components/Card";
import { RunRow } from "../../components/runs/RunRow";
import { RecordStoreDisabledPage, RecordStoreUnreachablePage, RecoveryState } from "../../components/RecoveryState";
import { ApiError, isRecordStoreUnreachable } from "../../hooks/useApi";
import type { FleetRunRow } from "../../types";
import { NeedsAttentionTile, PassRateTile, RunningTile, RunsTile } from "../pipelines/FleetCounters";
import { RunTable } from "../pipelines/RunTable";
import { TenantRail } from "../pipelines/TenantRail";
import { DEFAULT_FEED_QUERY } from "../pipelines/feedQuery";
import { useFleetRuns } from "../pipelines/useFleetRuns";
import { useFleetLive } from "../pipelines/useFleetLive";
import { usePipelinesOverview } from "../pipelines/usePipelinesOverview";
import styles from "./PipelinesSection.module.css";

/** TENANT_LIST_LIMIT caps the tenant rail of the Overview. */
const TENANT_LIST_LIMIT = 8;
/** LATEST_RUNS caps the latest-runs card of the Overview. */
const LATEST_RUNS = 5;

/**
 * PipelinesSection is the Pipelines half of the Overview. It owns its reads and
 * its error states. The latest runs share the command center's default feed.
 */
export function PipelinesSection() {
  const q = usePipelinesOverview();
  const tenants = q.data?.tenants ?? [];
  const ready = q.isSuccess && tenants.length > 0;
  const feed = useFleetRuns(DEFAULT_FEED_QUERY, ready);
  useFleetLive(DEFAULT_FEED_QUERY, ready);

  const head = (
    <div className={styles.sectionHead}>
      <h2 id="overview-pipelines" className={styles.sectionTitle}>Pipelines</h2>
      <Link to="/pipelines" className={styles.sectionLink}>
        Open pipelines <ChevronRight size={14} aria-hidden="true" />
      </Link>
    </div>
  );

  if (q.isPending) {
    return (
      <section className={styles.section} aria-labelledby="overview-pipelines">
        {head}
        <div className={styles.metrics}>
          {[1, 2, 3, 4].map((i) => (
            <div key={i} className={styles.skeletonMetric} data-testid="metric-skeleton" />
          ))}
        </div>
      </section>
    );
  }

  if (q.isError) {
    let body;
    if (isRecordStoreUnreachable(q.error)) {
      body = <RecordStoreUnreachablePage compact onRetry={() => void q.refetch()} />;
    } else if (q.error instanceof ApiError && q.error.status === 503) {
      body = <RecordStoreDisabledPage compact />;
    } else {
      body = (
        <RecoveryState
          compact
          kind="error"
          title="Unable to load pipelines"
          message="Varroa could not load the pipelines overview."
          onRetry={() => void q.refetch()}
        />
      );
    }
    return (
      <section className={styles.section} aria-labelledby="overview-pipelines">
        {head}
        {body}
      </section>
    );
  }

  if (tenants.length === 0) {
    return (
      <section className={styles.section} aria-labelledby="overview-pipelines">
        {head}
        <RecoveryState
          compact
          kind="403"
          title="No pipelines access"
          message="No Tenant grants you pipelines access. Ask a Tenant admin to add you."
        />
      </section>
    );
  }

  const data = q.data;
  const rest = tenants.length - TENANT_LIST_LIMIT;
  return (
    <section className={styles.section} aria-labelledby="overview-pipelines">
      {head}
      <div className={styles.metrics}>
        <RunsTile totals={data.totals} />
        <PassRateTile totals={data.totals} />
        <RunningTile totals={data.totals} />
        <NeedsAttentionTile attention={data.attention} />
      </div>
      <div className={styles.body}>
        <Card title="Tenants">
          <TenantRail tenants={tenants.slice(0, TENANT_LIST_LIMIT)} localCluster={data.localCluster} />
          {rest > 0 && (
            <div className={styles.more}>
              and {rest} more · <Link to="/pipelines">View all tenants</Link>
            </div>
          )}
        </Card>
        <Card title="Latest runs">
          <LatestRuns feed={feed} localCluster={data.localCluster} />
        </Card>
      </div>
    </section>
  );
}

/** LatestRuns shows the newest runs of the shared default feed. */
function LatestRuns({
  feed,
  localCluster,
}: {
  feed: ReturnType<typeof useFleetRuns>;
  localCluster: string;
}) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(t);
  }, []);

  if (feed.isError) {
    return (
      <RecoveryState
        compact
        kind="error"
        title="Unable to load runs"
        message="Varroa could not load the run feed."
        onRetry={() => void feed.refetch()}
      />
    );
  }
  if (feed.isPending) return <div className={styles.note}>Loading runs…</div>;
  const rows: FleetRunRow[] = feed.data?.pages.flatMap((p) => p.items) ?? [];
  if (rows.length === 0) return <div className={styles.note}>No runs yet.</div>;
  return (
    <RunTable>
      {rows.slice(0, LATEST_RUNS).map((r) => (
        <RunRow key={r.runId} run={r} now={now} localCluster={localCluster} />
      ))}
    </RunTable>
  );
}
