import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams, useSearchParams, Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getTeamCell, listPipelines, listRunners } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { useEventStream } from "../../hooks/useEventStream";
import { ForbiddenPage, GenericErrorPage, NotFoundPage, RecordStoreDisabledPage } from "../../components/RecoveryState";
import { Card } from "../../components/Card";
import { age } from "../../components/activityTimeline.util";
import type { RunNotification, RunnerEntry, RunningRun } from "../../types";
import JobTree, { formatDuration } from "./JobTree";
import TeamCellCard, { CELL_STATE_LABELS } from "./TeamCellCard";
import { buildJobTree, defaultOpenPaths, visibleRows } from "./jobTree";
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
      <div className={styles.tileStage}>{run.currentStage ?? "Queued"}</div>
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

export default function TeamHome() {
  const { team } = useParams<{ team: string }>();
  const [searchParams, setSearchParams] = useSearchParams();
  const queryClient = useQueryClient();
  const { data, isLoading, error } = useQuery({
    queryKey: ["pipelines", team],
    queryFn: () => listPipelines(team!),
    enabled: !!team,
  });

  // The team's stream carries every run of every tenant of that team.
  const { lastEvent, readyState } = useEventStream<RunNotification>(
    team ? `/pipelines/${enc(team)}/stream` : null,
    team ? `pipelines:${team}` : null,
    ["run", "stage"],
  );

  const invalidateTeam = useCallback(() => {
    void queryClient.invalidateQueries({ queryKey: ["pipelines", team] });
  }, [queryClient, team]);

  // The run-event effect reads the invalidator through this ref, so it runs
  // for one frame only and never replays a frame a team change leaves
  // behind.
  const invalidateTeamRef = useRef(invalidateTeam);
  invalidateTeamRef.current = invalidateTeam;

  // One read holds the running strip and the aggregates both, so the page
  // refetches that read and renders no payload field. A reconnect
  // refetches too, because the stream replays nothing.
  useEffect(() => {
    if (readyState === "open") invalidateTeam();
  }, [readyState, invalidateTeam]);

  const lastRunEventAtRef = useRef(0);
  // A new team starts a new window.
  useEffect(() => {
    lastRunEventAtRef.current = 0;
  }, [team]);
  useEffect(() => {
    // The team read carries no stage value, so a stage frame says nothing.
    if (lastEvent?.type !== "run") return;
    const now = Date.now();
    if (now - lastRunEventAtRef.current < RUN_EVENT_WINDOW_MS) return;
    lastRunEventAtRef.current = now;
    invalidateTeamRef.current();
  }, [lastEvent]);

  const runnersQuery = useQuery({
    queryKey: ["runners", team],
    queryFn: () => listRunners(team!),
    enabled: !!team,
  });
  // This call shares its query key with TeamCellCard's own call, so React
  // Query dedupes the two into one request.
  const cellQuery = useQuery({
    queryKey: ["team-cell", team],
    queryFn: () => getTeamCell(team!),
    enabled: !!team,
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

  if (isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading pipelines…</div>
      </div>
    );
  }

  if (error instanceof ApiError && error.status === 403) return <ForbiddenPage />;
  if (error instanceof ApiError && error.status === 404) return <NotFoundPage />;
  if (error instanceof ApiError && error.status === 503) return <RecordStoreDisabledPage />;
  if (error) return <GenericErrorPage />;
  if (!data) return <NotFoundPage />;

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <h1 className={styles.pageTitle}>{team}</h1>
        <div className={styles.pageDesc}>Pipelines for this team</div>
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

      {/* The card stays mounted while the disclosure is closed, so its own
          30 second read keeps running underneath. */}
      <div id="team-cell-disclosure" className={styles.cellDisclosure} hidden={!cellOpen}>
        <TeamCellCard team={team!} />
      </div>

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
              <JobTree team={team!} rows={rows} openPaths={openPaths} onToggle={toggleFold} />
            </tbody>
          </table>
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
        </Card>
      )}
    </div>
  );
}
