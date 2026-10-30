import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { getPipelinesOverview } from "../../api/client";
import { Button } from "../../components/Button";
import { RecordStoreDisabledPage, RecoveryState } from "../../components/RecoveryState";
import { useAssistant } from "../../components/assistant/AssistantProvider";
import { ApiError } from "../../hooks/useApi";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { canAccessTenantPipeline, usePermissions } from "../../hooks/usePermissions";
import { AttentionStrip } from "./AttentionStrip";
import { FleetCounters } from "./FleetCounters";
import { NewPipelinePanel } from "./NewPipelinePanel";
import { RunFeed } from "./RunFeed";
import { TenantRail } from "./TenantRail";
import { indexTitle } from "./pageTitles";
import { useFeedQuery, type FeedQuery } from "./feedQuery";
import { useFleetLive } from "./useFleetLive";
import styles from "./PipelinesIndex.module.css";

/**
 * PipelinesIndex is the /pipelines command center. It shows the attention
 * strip, the counters, the tenant rail, and the run feed over every tenant
 * the caller can read. A single tenant is selected in the view and not
 * written to the URL, so the page never redirects.
 */
export default function PipelinesIndex() {
  const overviewQuery = useQuery({
    queryKey: ["pipelines-overview"],
    queryFn: getPipelinesOverview,
    refetchInterval: 30_000,
  });
  const overview = overviewQuery.data;
  const perms = usePermissions().data;
  const assistant = useAssistant();
  const [storedQuery, setStoredQuery] = useFeedQuery();
  const [showNew, setShowNew] = useState(false);

  useDocumentTitle(indexTitle());

  // One name answers one rail row. The BFF sends each name once.
  const names = useMemo(() => (overview?.tenants ?? []).map((t) => t.name), [overview]);
  const single = names.length === 1 ? names[0] : undefined;
  // A single tenant is the view's tenant, but the URL keeps no tenant for it.
  const query = useMemo<FeedQuery>(
    () => (single !== undefined && storedQuery.tenant.length === 0 ? { ...storedQuery, tenant: [single] } : storedQuery),
    [storedQuery, single],
  );
  const onQuery = (next: FeedQuery) => {
    const onlySingle = single !== undefined && next.tenant.length === 1 && next.tenant[0] === single;
    setStoredQuery(onlySingle ? { ...next, tenant: [] } : next);
  };
  const onPreset = (p: Partial<FeedQuery>) => {
    const next: FeedQuery = { ...query, ...p };
    if (p.result) next.status = "";
    if (p.status) next.result = "";
    onQuery(next);
  };

  const live = useFleetLive(query, names.length > 0);

  if (overviewQuery.isPending) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading pipelines…</div>
      </div>
    );
  }

  if (overviewQuery.isError) {
    if (overviewQuery.error instanceof ApiError && overviewQuery.error.status === 503) {
      return <RecordStoreDisabledPage />;
    }
    return (
      <RecoveryState
        kind="error"
        title="Unable to load pipelines"
        message="Varroa could not load the pipelines overview."
        onRetry={() => void overviewQuery.refetch()}
      />
    );
  }

  if (!overview || names.length === 0) {
    return (
      <RecoveryState
        kind="403"
        title="No pipelines access"
        message="No Tenant grants you pipelines access. Ask a Tenant admin to add you."
      />
    );
  }

  const writable = names.filter((n) => canAccessTenantPipeline(perms, n, "write", ""));
  const canNew = assistant.available && writable.length > 0;
  const tenantWord = names.length === 1 ? "tenant" : "tenants";
  const clusterWord = overview.clusters === 1 ? "cluster" : "clusters";

  return (
    <div className={styles.page}>
      <div className={styles.head}>
        <div className={styles.headText}>
          <h1 className={styles.title}>Pipelines</h1>
          <p className={styles.desc}>
            {`${names.length} ${tenantWord} you can read · ${overview.clusters} ${clusterWord} · runs from every tenant, newest first`}
          </p>
        </div>
        {canNew && (
          <Button variant="primary" onClick={() => setShowNew((open) => !open)}>
            New pipeline
          </Button>
        )}
      </div>

      {showNew && canNew && <NewPipelinePanel tenants={writable} />}

      <AttentionStrip overview={overview} onPreset={onPreset} />
      <FleetCounters overview={overview} />

      <div className={styles.body}>
        <aside className={styles.rail}>
          <TenantRail
            tenants={overview.tenants}
            localCluster={overview.localCluster}
            selected={query.tenant}
            onSelect={(selected) => onQuery({ ...query, tenant: selected })}
            showAll={single === undefined}
          />
        </aside>
        <div className={styles.feed}>
          <RunFeed
            query={query}
            onQuery={onQuery}
            tenants={names}
            localCluster={overview.localCluster}
            live={live}
            onNewPipeline={canNew ? () => setShowNew(true) : undefined}
          />
        </div>
      </div>
    </div>
  );
}
