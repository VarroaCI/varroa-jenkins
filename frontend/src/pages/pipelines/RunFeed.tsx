import { useEffect, useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { cancelRun, listFleetRuns, rerunRun } from "../../api/client";
import { FilterBar, FilterSelect, SearchBox, type FilterOption } from "../../components/FilterBar";
import { SortableHeader, nextSort, type SortOrder } from "../../components/SortableHeader";
import { RunRow } from "../../components/runs/RunRow";
import { RecoveryState } from "../../components/RecoveryState";
import { Button } from "../../components/Button";
import { useToast } from "../../components/Toast";
import { age } from "../../components/activityTimeline.util";
import type { FleetRunRow, FleetRunSort } from "../../types";
import {
  DEFAULT_FEED_QUERY,
  applyStatusSelect,
  feedQueryIsDefault,
  statusSelectValue,
  writeFeedQuery,
  type FeedQuery,
} from "./feedQuery";
import { feedParams } from "./feedLive";
import styles from "./RunFeed.module.css";
import tableStyles from "../tables.module.css";

/** FeedLive is the live state the page hands the feed. */
export interface FeedLive {
  updatedAt: number | null;
  newRuns: number;
  onShowNew: () => void;
}

export interface RunFeedProps {
  query: FeedQuery;
  onQuery: (next: FeedQuery) => void;
  /** The rail's tenant names, in rail order. */
  tenants: string[];
  localCluster: string;
  live: FeedLive;
  /** Opens the new pipeline panel from the empty state. */
  onNewPipeline?: () => void;
}

/** SORT_FIRST is the order a column takes on its first click. */
const SORT_FIRST: Record<FleetRunSort, SortOrder> = {
  started: "desc",
  duration: "desc",
  tenant: "asc",
  job: "asc",
  status: "asc",
  actor: "asc",
};

const TRIGGER_OPTIONS: FilterOption[] = [
  { value: "pullRequest", label: "Pull request" },
  { value: "push", label: "Push" },
  { value: "cron", label: "Cron" },
  { value: "manual", label: "Manual" },
  { value: "upstream", label: "Upstream" },
  { value: "webhook", label: "Webhook" },
  { value: "replay", label: "Replay" },
  { value: "restart", label: "Restart" },
  { value: "other", label: "Other" },
];

/** SINCE_OPTIONS are the window choices. Each value is a Go duration that the BFF accepts. */
const SINCE_OPTIONS: FilterOption[] = [
  { value: "24h", label: "Last 24 hours" },
  { value: "168h", label: "Last 7 days" },
];

const STATUS_OPTIONS: FilterOption[] = [
  { value: "QUEUED", label: "Queued" },
  { value: "RUNNING", label: "Running" },
  { value: "WAITING_INPUT", label: "Waiting for input" },
  { value: "SUCCESS", label: "Success" },
  { value: "FAILURE", label: "Failure" },
  { value: "UNSTABLE", label: "Unstable" },
  { value: "ABORTED", label: "Aborted" },
  { value: "INTERRUPTED", label: "Interrupted" },
];

/** distinctWith answers the distinct non-empty values of a list, with the active value kept. */
function distinctWith(values: (string | undefined)[], active: string): FilterOption[] {
  const set = new Set<string>();
  for (const v of values) if (v) set.add(v);
  if (active) set.add(active);
  return Array.from(set)
    .sort((a, b) => a.localeCompare(b))
    .map((v) => ({ value: v, label: v }));
}

/**
 * RunFeed is the fleet run table: filters, sortable headers, keyset paging,
 * and the row actions. The URL holds the view, and every change of the view
 * starts a new read, so no cursor survives a change.
 */
export function RunFeed({ query, onQuery, tenants, localCluster, live, onNewPipeline }: RunFeedProps) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const [now, setNow] = useState(() => Date.now());
  const [filtersOpen, setFiltersOpen] = useState(false);

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(t);
  }, []);

  const feed = useInfiniteQuery({
    queryKey: ["fleet-runs", writeFeedQuery(query).toString()],
    queryFn: ({ pageParam }) => listFleetRuns({ ...feedParams(query), cursor: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (last) => last.nextCursor,
  });

  const rows = useMemo<FleetRunRow[]>(() => feed.data?.pages.flatMap((p) => p.items) ?? [], [feed.data]);

  const rerun = useMutation({
    mutationFn: (r: FleetRunRow) => rerunRun(r.runId),
    onSuccess: () => {
      toast("Re-run requested");
      void queryClient.invalidateQueries({ queryKey: ["fleet-runs"] });
    },
    onError: () => toast("Re-run failed"),
  });
  const cancel = useMutation({
    mutationFn: (r: FleetRunRow) => cancelRun(r.runId),
    onSuccess: () => {
      toast("Cancel requested");
      void queryClient.invalidateQueries({ queryKey: ["fleet-runs"] });
    },
    onError: () => toast("Cancel failed"),
  });

  // The search box holds a draft, and it commits to the URL after a pause.
  // The URL never changes on each keystroke.
  const [draftQ, setDraftQ] = useState(query.q);
  const queryRef = useRef(query);
  queryRef.current = query;
  const onQueryRef = useRef(onQuery);
  onQueryRef.current = onQuery;
  useEffect(() => {
    setDraftQ(query.q);
  }, [query.q]);
  useEffect(() => {
    if (draftQ === queryRef.current.q) return;
    const t = setTimeout(() => onQueryRef.current({ ...queryRef.current, q: draftQ }), 400);
    return () => clearTimeout(t);
  }, [draftQ]);

  const jobOptions = distinctWith(
    rows.map((r) => r.jobPath),
    query.job,
  );
  const branchOptions = distinctWith(
    rows.map((r) => r.branch),
    query.branch,
  );
  const actorOptions = distinctWith(
    rows.map((r) => r.actor),
    query.actor,
  );
  const tenantOptions: FilterOption[] = tenants.map((t) => ({ value: t, label: t }));

  const onHeader = (key: FleetRunSort) => {
    const next = nextSort(query.sort, query.order, key, SORT_FIRST[key]);
    onQuery({ ...query, sort: next.key, order: next.order });
  };

  const updatedText = live.updatedAt === null ? "…" : age(new Date(live.updatedAt).toISOString(), { now });
  // A single tenant is implicit in the view. The URL keeps no tenant for it,
  // so it does not make the view a filtered one.
  const implicitTenant = tenants.length === 1 && query.tenant.length === 1 && query.tenant[0] === tenants[0];
  const isDefault = feedQueryIsDefault(implicitTenant ? { ...query, tenant: [] } : query);
  const clearFeed = () => {
    setDraftQ("");
    onQuery(DEFAULT_FEED_QUERY);
  };

  return (
    <section className={styles.feed} aria-label="Run feed">
      <div className={styles.toolbar}>
        <div className={styles.toolbarTop}>
          <span className={styles.count}>{rows.length} runs loaded</span>
          <span className={styles.live}>live · updated {updatedText}</span>
        </div>
        {live.newRuns > 0 && (
          <button type="button" className={styles.banner} onClick={live.onShowNew}>
            {live.newRuns === 1 ? "1 new run" : `${live.newRuns} new runs`}
          </button>
        )}
        <button
          type="button"
          className={styles.filtersToggle}
          aria-expanded={filtersOpen}
          aria-controls="run-feed-filters"
          onClick={() => setFiltersOpen((open) => !open)}
        >
          Filters
        </button>
        <div id="run-feed-filters" className={filtersOpen ? styles.filtersOpen : styles.filters}>
          <FilterBar onClear={() => clearFeed()} clearDisabled={isDefault}>
            <FilterSelect
              id="feed-tenant"
              label="Tenant"
              value={query.tenant[0] ?? ""}
              options={tenantOptions}
              onChange={(v) => onQuery({ ...query, tenant: v ? [v] : [] })}
            />
            <FilterSelect
              id="feed-job"
              label="Job"
              value={query.job}
              options={jobOptions}
              onChange={(v) => onQuery({ ...query, job: v })}
            />
            <FilterSelect
              id="feed-trigger"
              label="Trigger"
              value={query.trigger}
              options={TRIGGER_OPTIONS}
              onChange={(v) => onQuery({ ...query, trigger: v })}
            />
            <FilterSelect
              id="feed-status"
              label="Status"
              value={statusSelectValue(query)}
              options={STATUS_OPTIONS}
              onChange={(v) => onQuery(applyStatusSelect(query, v))}
            />
            <FilterSelect
              id="feed-branch"
              label="Branch"
              value={query.branch}
              options={branchOptions}
              onChange={(v) => onQuery({ ...query, branch: v })}
            />
            <FilterSelect
              id="feed-actor"
              label="Actor"
              value={query.actor}
              options={actorOptions}
              onChange={(v) => onQuery({ ...query, actor: v })}
            />
            <FilterSelect
              id="feed-since"
              label="Since"
              allLabel="Any time"
              value={query.since}
              options={
                query.since !== "" && !SINCE_OPTIONS.some((o) => o.value === query.since)
                  ? [...SINCE_OPTIONS, { value: query.since, label: query.since }]
                  : SINCE_OPTIONS
              }
              onChange={(v) => onQuery({ ...query, since: v })}
            />
            <SearchBox id="feed-q" label="Search" value={draftQ} onChange={setDraftQ} placeholder="Job, branch, or commit" />
          </FilterBar>
        </div>
      </div>

      {feed.isError ? (
        <RecoveryState
          kind="error"
          title="Unable to load runs"
          message="Varroa could not load the run feed."
          onRetry={() => void feed.refetch()}
        />
      ) : feed.isPending ? (
        <div className={styles.loading}>Loading runs…</div>
      ) : rows.length === 0 ? (
        isDefault ? (
          <div className={styles.empty}>
            <p>No runs yet</p>
            {onNewPipeline && (
              <Button variant="primary" onClick={onNewPipeline}>
                New pipeline
              </Button>
            )}
          </div>
        ) : (
          <div className={styles.empty}>
            <p>No runs match these filters.</p>
            <Button onClick={() => clearFeed()}>Clear</Button>
          </div>
        )
      ) : (
        <div className={tableStyles.tableScroll}>
          <table className={`${tableStyles.dataTable} ${styles.table}`}>
            <thead>
              <tr>
                <SortableHeader<FleetRunSort> label="Status" sortKey="status" activeKey={query.sort} order={query.order} onSort={onHeader} />
                <SortableHeader<FleetRunSort> label="Run" sortKey="job" activeKey={query.sort} order={query.order} onSort={onHeader} />
                <SortableHeader<FleetRunSort> label="Tenant" sortKey="tenant" activeKey={query.sort} order={query.order} onSort={onHeader} />
                <SortableHeader<FleetRunSort> label="Actor" sortKey="actor" activeKey={query.sort} order={query.order} onSort={onHeader} />
                <th scope="col">Branch</th>
                <SortableHeader<FleetRunSort> label="Started" sortKey="started" activeKey={query.sort} order={query.order} onSort={onHeader} />
                <SortableHeader<FleetRunSort> label="Duration" sortKey="duration" activeKey={query.sort} order={query.order} onSort={onHeader} />
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <RunRow
                  key={r.runId}
                  run={r}
                  now={now}
                  localCluster={localCluster}
                  onRerun={(run) => rerun.mutate(run)}
                  onCancel={(run) => cancel.mutate(run)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {feed.hasNextPage && (
        <div className={styles.more}>
          <Button onClick={() => void feed.fetchNextPage()} disabled={feed.isFetchingNextPage}>
            Show older runs
          </Button>
        </div>
      )}
    </section>
  );
}
