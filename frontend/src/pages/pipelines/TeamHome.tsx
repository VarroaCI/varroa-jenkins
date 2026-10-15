import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams, useSearchParams, Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getTeam, getTeamCell, listFleetLocks, listPipelines, listRunners, teamRunStreamUrl } from "../../api/client";
import type { TeamEntry } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { useEventStream } from "../../hooks/useEventStream";
import {
  ForbiddenPage,
  GenericErrorPage,
  NoPipelinesPage,
  NotFoundPage,
  RecordStoreDisabledPage,
} from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import { age } from "../../components/activityTimeline.util";
import { holderOf, holdsLock } from "../../lib/fleetLocks";
import type { FleetLock, RunNotification, RunnerEntry, RunningRun } from "../../types";
import JobTree, { formatDuration } from "./JobTree";
import { breakablePath } from "./breakablePath";
import TeamCellCard, { CELL_STATE_LABELS } from "./TeamCellCard";
import { buildJobTree, defaultOpenPaths, visibleRows } from "./jobTree";
import { teamTitle } from "./pageTitles";
import styles from "./TeamHome.module.css";

/** DEFAULT_OPEN_LEVELS is how many folder levels a first load unfolds. The
 * value counts levels, so two means node depth 0 and node depth 1. */
const DEFAULT_OPEN_LEVELS = 2;

/** RUN_EVENT_WINDOW_MS bounds the team read to one request per window. One
 * build writes in bursts, and one read answers every event of a burst. */
const RUN_EVENT_WINDOW_MS = 2000;

const enc = (s: string) => encodeURIComponent(s);

function jobHref(team: string, jobPath: string): string {
  return `/pipelines/${enc(team)}/${enc(jobPath)}`;
}

function runHref(team: string, jobPath: string, runId: string): string {
  return `${jobHref(team, jobPath)}/${enc(runId)}`;
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
 * RunningTile renders one Running now tile. The stage line names the run's
 * stage in progress. A run that has started no stage reads "Starting", and
 * a run that has not started reads "Queued".
 */
function RunningTile({ team, run }: { team: string; run: RunningRun }) {
  const started = run.startedAt ?? run.queuedAt;
  const elapsedMs = started ? Date.now() - Date.parse(started) : 0;
  const pct = run.typicalDurationMs
    ? Math.max(0, Math.min(100, (elapsedMs / run.typicalDurationMs) * 100))
    : 0;
  return (
    <Link to={runHref(team, run.jobPath, run.runId)} className={styles.tile}>
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

export default function TeamHome() {
  const { team } = useParams<{ team: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const queryClient = useQueryClient();
  const { data, isLoading, error } = useQuery({
    queryKey: ["pipelines", team],
    queryFn: () => listPipelines(team!),
    enabled: !!team,
    // A Team that waits for its first cell answers Not found until the
    // operator places it. The read then polls, so the page fills in alone.
    refetchInterval: (query) => {
      const notFound = query.state.error instanceof ApiError && query.state.error.status === 404;
      const probe = queryClient.getQueryData<TeamEntry>(["pipelines-team-probe", team]);
      return notFound && probe?.pipelines ? 30_000 : false;
    },
  });

  // The main read answers Not found for a Team with no placed cell and for a
  // Team the caller cannot read. The Team read tells the two apart.
  const mainReady = !!team && data !== undefined;
  const mainNotFound = !!team && data === undefined && error instanceof ApiError && error.status === 404;
  const teamProbe = useQuery({
    queryKey: ["pipelines-team-probe", team],
    queryFn: () => getTeam(team!),
    enabled: mainNotFound,
    retry: false,
  });
  const waiting = mainNotFound && !!teamProbe.data?.pipelines;

  // The team's stream carries every run and every lock of every tenant of
  // that team. It opens only after the main read succeeds.
  const { lastEvent, readyState } = useEventStream<RunNotification>(
    mainReady ? teamRunStreamUrl(team!) : null,
    mainReady ? `pipelines:${team}` : null,
    ["run", "stage", "lock"],
  );

  // One window drives both reads: a run frame and a stage frame change a
  // tile, and a lock frame changes a lock row. The page reads its own
  // lists and it renders no payload field.
  const refreshReads = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["pipelines", team] });
    void queryClient.invalidateQueries({ queryKey: ["fleet-locks", team] });
  }, [queryClient, team]);

  // The run-event effect reads the refresher through this ref, so it runs
  // for one frame only and never replays a frame a team change leaves
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
  // A new team starts a new window, and it cancels the timer the old team
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
  }, [team]);
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
    queryKey: ["runners", team],
    queryFn: () => listRunners(team!),
    enabled: mainReady,
  });
  // The team stream carries a lock frame for this tenant's own locks only.
  // A lock under this tenant's prefix that another tenant's run holds
  // publishes on the holder's subject, so the card also polls on the cells
  // cadence.
  const locksQuery = useQuery({
    queryKey: ["fleet-locks", team],
    queryFn: () => listFleetLocks(team!),
    enabled: mainReady,
    refetchInterval: 15_000,
  });
  // This call shares its query key with TeamCellCard's own call, so React
  // Query dedupes the two into one request. A waiting Team reads it too,
  // because the pill names the state the Team waits in.
  const cellQuery = useQuery({
    queryKey: ["team-cell", team],
    queryFn: () => getTeamCell(team!),
    enabled: mainReady || waiting,
    refetchInterval: 30_000,
  });
  const [cellOpen, setCellOpen] = useState(false);

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

  useDocumentTitle(team ? teamTitle(team) : null);

  if (isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading pipelines…</div>
      </div>
    );
  }

  if (error instanceof ApiError && error.status === 403) return <ForbiddenPage />;
  if (error instanceof ApiError && error.status === 503) return <RecordStoreDisabledPage />;

  // The page head and the cell disclosure are the same on every branch that
  // names a Team, so both come from one element.
  const pageHead = (
    <div className={styles.pageHead}>
      <h1 className={styles.pageTitle}>{team}</h1>
      <div className={styles.pageDesc}>Pipelines for this team</div>
      {data?.retention && (
        <div className={styles.retention} data-testid="team-retention">
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
          the pill then, as TeamCellCard hides its own card. */}
      {cellQuery.data && !cellQuery.isError && cellQuery.data.state !== "notCellHosted" && (
        <button
          type="button"
          className={pillClassName(cellQuery.data.state)}
          aria-expanded={cellOpen}
          aria-controls="team-cell-disclosure"
          onClick={() => setCellOpen((v) => !v)}
        >
          {CELL_STATE_LABELS[cellQuery.data.state]}
          {cellQuery.data.cell ? ` · ${cellQuery.data.cell}` : ""}
        </button>
      )}
    </div>
  );

  // The card stays mounted while the disclosure is closed, so its own
  // 30 second read keeps running underneath.
  const cellDisclosure = (
    <div id="team-cell-disclosure" className={styles.cellDisclosure} hidden={!cellOpen}>
      <TeamCellCard team={team!} />
    </div>
  );

  if (mainNotFound) {
    if (teamProbe.isLoading) {
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
            <div>This team has no cell yet.</div>
            <div className={styles.emptyNote}>
              Its pipelines show here after Varroa places it on a cell. Select the status above to read the reason.
            </div>
          </div>
        </div>
      );
    }
    // The Team read answers only to a caller who can read the Team, so a
    // Team with no pipelines shows its own page only to that caller.
    if (teamProbe.data) return <NoPipelinesPage />;
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
        <div className={styles.empty}>Nothing is running.</div>
      ) : (
        <div className={styles.tileGrid}>
          {data.runningNow.map((run) => (
            <RunningTile key={run.runId} team={team!} run={run} />
          ))}
        </div>
      )}

      <div className={styles.sectionTitle}>Pipelines</div>
      {rows.length === 0 ? (
        <div className={styles.empty}>
          <div>No pipelines yet.</div>
          <div className={styles.emptyNote}>This list shows only the jobs your access covers.</div>
        </div>
      ) : (
        <Card>
          <div className={styles.tableScroll}>
            <table className={styles.table}>
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
                  team={team!}
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
        <div className={styles.loading}>Loading locks…</div>
      ) : locksQuery.isError ? (
        <div className={styles.empty}>Unable to load locks.</div>
      ) : lockItems.length === 0 ? (
        <div className={styles.empty}>The tenant holds no lock and waits on none.</div>
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
        <div className={styles.loading}>Loading runners…</div>
      ) : runnersQuery.isError ? (
        <div className={styles.empty}>Unable to load runners.</div>
      ) : !runnersQuery.data || runnersQuery.data.length === 0 ? (
        <div className={styles.empty}>No runners registered for this team yet.</div>
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
