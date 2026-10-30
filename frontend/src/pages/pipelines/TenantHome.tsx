import { Fragment, useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useParams, useSearchParams, Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getTenant, getTenantCell, listFleetLocks, listPipelines, listRunners, tenantRunStreamUrl } from "../../api/client";
import type { TenantEntry } from "../../api/client";
import { ApiError, isRecordStoreUnreachable } from "../../hooks/useApi";
import { canAccessTenantPipeline, usePermissions } from "../../hooks/usePermissions";
import { useAssistant } from "../../components/assistant/AssistantProvider";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { useEventStream } from "../../hooks/useEventStream";
import {
  ForbiddenPage,
  GenericErrorPage,
  NoPipelinesPage,
  NotFoundPage,
  RecordStoreDisabledPage,
  RecordStoreUnreachablePage,
} from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import { age } from "../../components/activityTimeline.util";
import { useClusters, coreOf } from "../../hooks/useClusters";
import { controllerRoute } from "../../routing";
import { holderOf, holdsLock } from "../../lib/fleetLocks";
import type { FleetLock, RunNotification, RunnerEntry, RunningRun, TenantCell } from "../../types";
import JobTree, { formatDuration } from "./JobTree";
import { breakablePath } from "./breakablePath";
import TenantCellCard, { CELL_STATE_LABELS } from "./TenantCellCard";
import TenantAgentCard from "./TenantAgentCard";
import ProposePipelineCard from "./ProposePipelineCard";
import { buildJobTree, defaultOpenPaths, visibleRows } from "./jobTree";
import { tenantTitle } from "./pageTitles";
import styles from "./TenantHome.module.css";

/** DEFAULT_OPEN_LEVELS is how many folder levels a first load unfolds. The
 * value counts levels, so two means node depth 0 and node depth 1. */
const DEFAULT_OPEN_LEVELS = 2;

/** RUN_EVENT_WINDOW_MS bounds the tenant read to one request per window. One
 * build writes in bursts, and one read answers every event of a burst. */
const RUN_EVENT_WINDOW_MS = 2000;

const enc = (s: string) => encodeURIComponent(s);

function jobHref(tenant: string, jobPath: string): string {
  return `/pipelines/${enc(tenant)}/${enc(jobPath)}`;
}

function runHref(tenant: string, jobPath: string, runId: string): string {
  return `${jobHref(tenant, jobPath)}/${enc(runId)}`;
}

/** PILL_CLASS maps each cell-hosting state to the pill's colour pair. The
 * text carries the state, and the colour supports it only. */
const PILL_CLASS: Record<keyof typeof CELL_STATE_LABELS, string> = {
  placed: styles.cellPillOk,
  moving: styles.cellPillInfo,
  rehydrating: styles.cellPillInfo,
  unplaced: styles.cellPillWarn,
};

function pillClassName(state: keyof typeof CELL_STATE_LABELS): string {
  return PILL_CLASS[state];
}

/**
 * CellMeta names what the pill does not: a member cluster, the other tenants
 * on the cell, the free slots, a warning, and a link to the cell. It shows
 * nothing for a tenant with no cell.
 */
function CellMeta({ cell, coreName }: { cell: TenantCell; coreName?: string }) {
  const parts: ReactNode[] = [];
  if (cell.cluster && coreName !== undefined && cell.cluster !== coreName) {
    parts.push(
      <span key="member" className={styles.memberChip}>
        member cluster {cell.cluster}
      </span>,
    );
  }
  if (cell.otherTenantCount !== undefined) {
    const n = cell.otherTenantCount;
    parts.push(`${n} other tenant${n === 1 ? "" : "s"}`);
  }
  if (cell.freeSlots !== undefined) {
    const n = cell.freeSlots;
    parts.push(`${n} free slot${n === 1 ? "" : "s"}`);
  }
  if (cell.warning) {
    parts.push(
      <span key="warning" className={styles.cellWarn} title={cell.warning.reason}>
        {cell.warning.message}
      </span>,
    );
  }
  if (cell.canOpenCell && cell.cluster && cell.namespace && cell.cell) {
    parts.push(
      <Link key="open" to={`${controllerRoute(cell.cluster, cell.namespace, cell.cell)}?tab=cell`}>
        Open cell
      </Link>,
    );
  }
  if (parts.length === 0) return null;
  return (
    <span className={styles.cellMeta} data-testid="tenant-cell-meta">
      {parts.map((part, i) => (
        <Fragment key={i}>
          {i > 0 && " · "}
          {part}
        </Fragment>
      ))}
    </span>
  );
}

/**
 * RunningTile renders one Running now tile. The stage line names the run's
 * stage in progress. A run that has started no stage reads "Starting", and
 * a run that has not started reads "Queued".
 */
function RunningTile({ tenant, run }: { tenant: string; run: RunningRun }) {
  const started = run.startedAt ?? run.queuedAt;
  const elapsedMs = started ? Date.now() - Date.parse(started) : 0;
  const pct = run.typicalDurationMs
    ? Math.max(0, Math.min(100, (elapsedMs / run.typicalDurationMs) * 100))
    : 0;
  return (
    <Link to={runHref(tenant, run.jobPath, run.runId)} className={styles.tile}>
      <div className={styles.tileHead}>
        <span className={styles.tileJob}>{run.jobPath}</span>
        <span className={styles.tileBuild}>{run.buildNumber ? `#${run.buildNumber}` : "Queued"}</span>
      </div>
      <div className={styles.tileStage}>{run.currentStage ?? (run.startedAt ? "Starting" : "Queued")}</div>
      <div className={styles.tileMeta}>
        <span>{run.branch ?? "—"}</span>
        <span>{run.scmRevision ? run.scmRevision.slice(0, 7) : "—"}</span>
      </div>
      {run.changeSummary && <div className={styles.tileCommitMsg}>{run.changeSummary}</div>}
      <div className={styles.progressTrack}>
        <div className={styles.progressFill} style={{ width: `${pct}%` }} />
      </div>
      <div className={styles.tileTimes}>
        <span>{started ? age(started) : "queued"}</span>
        <span>usual {formatDuration(run.typicalDurationMs)}</span>
      </div>
    </Link>
  );
}

/**
 * RunnerRow renders one external runner's inventory row. It never reads
 * status.token: the dashboard is not a place a door credential appears
 * (design D12). C9 owns the Jenkins UI block, so this row carries no
 * Jenkins link.
 */
function RunnerRow({ item }: { item: RunnerEntry }) {
  const labels = item.spec.labels ?? [];
  return (
    <tr>
      <td className={styles.nameCell}>{item.name}</td>
      <td>{item.spec.launcher}</td>
      <td>{labels.length > 0 ? labels.join(", ") : <span className={styles.dim}>—</span>}</td>
      <td>{item.status.cell || <span className={styles.dim}>—</span>}</td>
      <td>
        <span className={item.status.connected ? styles.connectedYes : styles.connectedNo}>
          {item.status.connected ? "Connected" : "Not connected"}
        </span>
      </td>
      <td>{item.status.lastSeen ? age(item.status.lastSeen) : <span className={styles.dim}>—</span>}</td>
    </tr>
  );
}

/**
 * LockRow renders one lock. A held or a releasing row names its own run. A
 * waiting row names the row of the same lock name that holds the lock, and
 * the page groups that row from the list it already holds. When this list
 * carries no holder row, the row states that the lock is held elsewhere.
 * The row shows no queue position, because no read answers one.
 */
function LockRow({ items, row }: { items: FleetLock[]; row: FleetLock }) {
  const waiting = row.state === "waiting";
  const holder = waiting ? holderOf(items, row) : holdsLock(row) ? row : undefined;
  const since = waiting ? row.waitingSince : row.acquiredAt;
  return (
    <tr>
      <td className={styles.nameCell}>{row.name}</td>
      <td>{row.state}</td>
      <td>
        {holder ? (
          <HolderRun row={holder} />
        ) : waiting ? (
          <span className={styles.dim}>held elsewhere</span>
        ) : (
          <span className={styles.dim}>—</span>
        )}
      </td>
      <td className={styles.lockSince}>
        {since ? age(since) : <span className={styles.dim}>—</span>}
      </td>
    </tr>
  );
}

/**
 * HolderRun names the run that holds a lock. The cell links to the run
 * when the response carries a runLink. A lock the caller cannot read
 * states its job path as plain text.
 */
function HolderRun({ row }: { row: FleetLock }) {
  if (!row.runLink) return <span className={styles.lockJob}>{breakablePath(row.jobPath)}</span>;
  return (
    <Link to={row.runLink} className={styles.lockJob}>
      {breakablePath(row.jobPath)}
    </Link>
  );
}

export default function TenantHome() {
  const { tenant } = useParams<{ tenant: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const queryClient = useQueryClient();
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["pipelines", tenant],
    queryFn: () => listPipelines(tenant!),
    enabled: !!tenant,
    // A Tenant that waits for its first cell answers Not found until the
    // operator places it. The read then polls, so the page fills in alone.
    refetchInterval: (query) => {
      const notFound = query.state.error instanceof ApiError && query.state.error.status === 404;
      const probe = queryClient.getQueryData<TenantEntry>(["pipelines-tenant-probe", tenant]);
      return notFound && probe?.pipelines ? 30_000 : false;
    },
  });

  // The main read answers Not found for a Tenant with no placed cell and for a
  // Tenant the caller cannot read. The Tenant read tells the two apart.
  const mainReady = !!tenant && data !== undefined;
  const mainNotFound = !!tenant && data === undefined && error instanceof ApiError && error.status === 404;
  const tenantProbe = useQuery({
    queryKey: ["pipelines-tenant-probe", tenant],
    queryFn: () => getTenant(tenant!),
    enabled: mainNotFound,
    retry: false,
  });
  const waiting = mainNotFound && !!tenantProbe.data?.pipelines;

  // The tenant's stream carries every run and every lock of every tenant of
  // that tenant. It opens only after the main read succeeds.
  const { lastEvent, readyState } = useEventStream<RunNotification>(
    mainReady ? tenantRunStreamUrl(tenant!) : null,
    mainReady ? `pipelines:${tenant}` : null,
    ["run", "stage", "lock"],
  );

  // One window drives both reads: a run frame and a stage frame change a
  // tile, and a lock frame changes a lock row. The page reads its own
  // lists and it renders no payload field.
  const refreshReads = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["pipelines", tenant] });
    void queryClient.invalidateQueries({ queryKey: ["fleet-locks", tenant] });
  }, [queryClient, tenant]);

  // The run-event effect reads the refresher through this ref, so it runs
  // for one frame only and never replays a frame a tenant change leaves
  // behind.
  const refreshReadsRef = useRef(refreshReads);
  refreshReadsRef.current = refreshReads;

  // A reconnect reads both lists again, because the stream replays
  // nothing.
  useEffect(() => {
    if (readyState === "open") refreshReads();
  }, [readyState, refreshReads]);

  const lastRunEventAtRef = useRef(0);
  const windowTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // A new tenant starts a new window, and it cancels the timer the old tenant
  // armed. An unmount cancels too, so no read lands on a page that has
  // left.
  useEffect(() => {
    lastRunEventAtRef.current = 0;
    return () => {
      if (windowTimerRef.current !== null) {
        clearTimeout(windowTimerRef.current);
        windowTimerRef.current = null;
      }
    };
  }, [tenant]);
  useEffect(() => {
    // A run frame and a stage frame both change a tile, and a run's first
    // stage row can land after the frame that started the run. A lock
    // frame changes a lock row. The page reads its lists once per window: a
    // frame outside the window reads at once, and a frame inside it arms
    // one timer that reads when the window ends, so no frame is lost.
    if (lastEvent?.type !== "run" && lastEvent?.type !== "stage" && lastEvent?.type !== "lock") {
      return;
    }
    const now = Date.now();
    const waitMs = RUN_EVENT_WINDOW_MS - (now - lastRunEventAtRef.current);
    if (waitMs <= 0) {
      // A window timer can still be pending here when it fires late, and
      // the read below covers the window it armed. Clear it first, so two
      // reads never land inside one window.
      if (windowTimerRef.current !== null) {
        clearTimeout(windowTimerRef.current);
        windowTimerRef.current = null;
      }
      lastRunEventAtRef.current = now;
      refreshReadsRef.current();
      return;
    }
    if (windowTimerRef.current !== null) return;
    windowTimerRef.current = setTimeout(() => {
      windowTimerRef.current = null;
      lastRunEventAtRef.current = Date.now();
      refreshReadsRef.current();
    }, waitMs);
  }, [lastEvent]);

  const runnersQuery = useQuery({
    queryKey: ["runners", tenant],
    queryFn: () => listRunners(tenant!),
    enabled: mainReady,
  });
  // The tenant stream carries a lock frame for this tenant's own locks only.
  // A lock under this tenant's prefix that another tenant's run holds
  // publishes on the holder's subject, so the card also polls on the cells
  // cadence.
  const locksQuery = useQuery({
    queryKey: ["fleet-locks", tenant],
    queryFn: () => listFleetLocks(tenant!),
    enabled: mainReady,
    refetchInterval: 15_000,
  });
  // This call shares its query key with TenantCellCard's own call, so React
  // Query dedupes the two into one request. A waiting Tenant reads it too,
  // because the pill names the state the Tenant waits in.
  const cellQuery = useQuery({
    queryKey: ["tenant-cell", tenant],
    queryFn: () => getTenantCell(tenant!),
    enabled: mainReady || waiting,
    refetchInterval: 30_000,
  });
  const [cellOpen, setCellOpen] = useState(false);
  const { data: clusters } = useClusters();
  const core = coreOf(clusters);

  const tree = useMemo(
    () => buildJobTree(data?.items ?? [], data?.runningNow ?? [], data?.tenantFolder ?? ""),
    [data],
  );

  // A URL that carries at least one "open" parameter, an empty one
  // included, names the whole fold set. Only a URL carrying none at all
  // falls back to the default, so a caller who folds everything writes
  // "?open=" and the page obeys it.
  const openPaths = useMemo(() => {
    if (searchParams.has("open")) {
      return new Set(searchParams.getAll("open").filter((value) => value !== ""));
    }
    return new Set(defaultOpenPaths(tree, DEFAULT_OPEN_LEVELS));
  }, [searchParams, tree]);

  const rows = useMemo(() => visibleRows(tree, openPaths), [tree, openPaths]);
  const assistant = useAssistant();
  const perms = usePermissions(assistant.available);
  // The proposal card shows on a placed pipelines tenant with no job, to a
  // writer, when the assistant is available.
  const showPropose =
    rows.length === 0 && data?.tenantFolder === tenant && assistant.available &&
    canAccessTenantPipeline(perms.data, tenant!, "write", "");

  const lockItems = useMemo(() => locksQuery.data?.items ?? [], [locksQuery.data]);

  // A fold is a view setting, not a navigation step, so it replaces the
  // current history entry. The browser back button then leaves the page
  // instead of stepping through every fold.
  const toggleFold = (path: string, next: boolean) => {
    const paths = new Set(openPaths);
    if (next) {
      paths.add(path);
    } else {
      paths.delete(path);
    }
    const params = new URLSearchParams(searchParams);
    params.delete("open");
    if (paths.size === 0) {
      params.append("open", "");
    } else {
      for (const value of paths) params.append("open", value);
    }
    setSearchParams(params, { replace: true });
  };

  useDocumentTitle(tenant ? tenantTitle(tenant) : null);

  if (isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading pipelines…</div>
      </div>
    );
  }

  if (error instanceof ApiError && error.status === 403) return <ForbiddenPage />;
  if (isRecordStoreUnreachable(error)) return <RecordStoreUnreachablePage onRetry={() => void refetch()} />;
  if (error instanceof ApiError && error.status === 503) return <RecordStoreDisabledPage />;

  // The page head and the cell disclosure are the same on every branch that
  // names a Tenant, so both come from one element.
  const pageHead = (
    <div className={styles.pageHead}>
      <h1 className={styles.pageTitle}>{tenant}</h1>
      <div className={styles.pageDesc}>Pipelines for this tenant</div>
      {data?.retention && (
        <div className={styles.retention} data-testid="tenant-retention">
          Build history kept {data.retention.effectiveDays} days
          {data.retention.tenantDays !== undefined &&
            data.retention.tenantDays < data.retention.storeDays &&
            ` (store limit ${data.retention.storeDays} days)`}
          {data.retention.jobs.length > 0 && (
            <ul className={styles.retentionJobs}>
              {data.retention.jobs.map((rule) => (
                <li key={rule.job}>
                  {rule.job}: {rule.effectiveDays} days
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {/* A failed refetch keeps the last data, so the isError test hides
          the pill then, as TenantCellCard hides its own card. */}
      {cellQuery.data && !cellQuery.isError && cellQuery.data.state !== "notCellHosted" && (
        <div className={styles.cellRow}>
          <button
            type="button"
            className={pillClassName(cellQuery.data.state)}
            aria-expanded={cellOpen}
            aria-controls="tenant-cell-disclosure"
            onClick={() => setCellOpen((v) => !v)}
          >
            {CELL_STATE_LABELS[cellQuery.data.state]}
            {cellQuery.data.cell ? ` · ${cellQuery.data.cell}` : ""}
          </button>
          {cellQuery.data.state !== "unplaced" && <CellMeta cell={cellQuery.data} coreName={core?.name} />}
        </div>
      )}
    </div>
  );

  // The card stays mounted while the disclosure is closed, so its own
  // 30 second read keeps running underneath.
  const cellDisclosure = (
    <div id="tenant-cell-disclosure" className={styles.cellDisclosure} hidden={!cellOpen}>
      <TenantCellCard tenant={tenant!} />
      <TenantAgentCard tenant={tenant!} />
    </div>
  );

  if (mainNotFound) {
    if (tenantProbe.isLoading) {
      return (
        <div className={styles.page}>
          <div className={styles.loading}>Loading pipelines…</div>
        </div>
      );
    }
    if (waiting) {
      return (
        <div className={styles.page}>
          {pageHead}
          {cellDisclosure}
          <div className={styles.empty}>
            <div>This tenant has no cell yet.</div>
            <div className={styles.emptyNote}>
              Its pipelines show here after Varroa places it on a cell. Select the status above to read the reason.
            </div>
          </div>
        </div>
      );
    }
    // The Tenant read answers only to a caller who can read the Tenant, so a
    // Tenant with no pipelines shows its own page only to that caller.
    if (tenantProbe.data) return <NoPipelinesPage />;
    return <NotFoundPage />;
  }

  if (error) return <GenericErrorPage />;
  if (!data) return <NotFoundPage />;

  return (
    <div className={styles.page}>
      {pageHead}
      {cellDisclosure}

      <div className={styles.sectionTitle}>Running now</div>
      {data.truncated && (
        <p className={styles.truncatedNote}>
          This view is bounded and may not show every running build.
        </p>
      )}
      {data.runningNow.length === 0 ? (
        <div className={styles.emptyLine}>Nothing is running.</div>
      ) : (
        <div className={styles.tileGrid}>
          {data.runningNow.map((run) => (
            <RunningTile key={run.runId} tenant={tenant!} run={run} />
          ))}
        </div>
      )}

      <div className={styles.sectionTitle}>Pipelines</div>
      {rows.length === 0 ? (
        <div className={styles.empty}>
          {showPropose ? <ProposePipelineCard tenant={tenant!} /> : <div>No pipelines yet.</div>}
          <div className={styles.emptyNote}>This list shows only the jobs your access covers.</div>
        </div>
      ) : (
        <Card>
          <div className={styles.tableScroll}>
            <table className={styles.jobsTable}>
              <thead>
                <tr>
                  <th>Pipeline</th>
                  <th>Health</th>
                  <th>Last build</th>
                  <th>Branch</th>
                  <th>Duration, last 8</th>
                  <th>Queue wait</th>
                  <th>When</th>
                </tr>
              </thead>
              <tbody>
                <JobTree
                  tenant={tenant!}
                  folder={data?.tenantFolder ?? ""}
                  rows={rows}
                  openPaths={openPaths}
                  onToggle={toggleFold}
                />
              </tbody>
            </table>
          </div>
        </Card>
      )}

      <div className={styles.sectionTitle}>Locks</div>
      {locksQuery.isLoading ? (
        <div className={styles.emptyLine}>Loading locks…</div>
      ) : locksQuery.isError ? (
        <div className={styles.emptyLine}>Unable to load locks.</div>
      ) : lockItems.length === 0 ? (
        <div className={styles.emptyLine}>The tenant holds no lock and waits on none.</div>
      ) : (
        <Card>
          <div className={styles.tableScroll}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Lock</th>
                  <th>State</th>
                  <th>Holder</th>
                  <th>Since</th>
                </tr>
              </thead>
              <tbody>
                {lockItems.map((row) => (
                  <LockRow key={`${row.name}/${row.runId}`} items={lockItems} row={row} />
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      <div className={styles.sectionTitle}>Runners</div>
      {runnersQuery.isLoading ? (
        <div className={styles.emptyLine}>Loading runners…</div>
      ) : runnersQuery.isError ? (
        <div className={styles.emptyLine}>Unable to load runners.</div>
      ) : !runnersQuery.data || runnersQuery.data.length === 0 ? (
        <div className={styles.emptyLine}>No runners. An operator registers a Runner for this tenant.</div>
      ) : (
        <Card>
          <div className={styles.tableScroll}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Launcher</th>
                  <th>Labels</th>
                  <th>Cell</th>
                  <th>Connected</th>
                  <th>Last seen</th>
                </tr>
              </thead>
              <tbody>
                {runnersQuery.data.map((item) => (
                  <RunnerRow key={item.name} item={item} />
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  );
}
