import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useLocation, useNavigate, useParams, useSearchParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, Keyboard, Repeat, RotateCcw, XCircle } from "lucide-react";
import {
  abortRunInput,
  cancelRun,
  getRun,
  getRunScript,
  listRunArtifacts,
  listRunChanges,
  listRuns,
  listRunTests,
  proceedRunInput,
  rerunRun,
  runLogUrl,
  teamRunStreamUrl,
  downloadRunArtifact,
} from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { useEventStream, type SSEEvent } from "../../hooks/useEventStream";
import { canAccessTenantPipeline, usePermissions } from "../../hooks/usePermissions";
import { useToast } from "../../components/Toast";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import { Tabs, tabPanelProps } from "../../components/Tabs";
import { ForbiddenPage, GenericErrorPage, RecoveryState } from "../../components/RecoveryState";
import type {
  RunCommandRefusal,
  RunDetail,
  RunLogEndEvent,
  RunLogGapEvent,
  RunLogLineEvent,
  RunLogLiveEvent,
  RunLogMetaEvent,
  RunLogStageEvent,
  RunNotification,
  RunStage,
} from "../../types";
import { stripTenantFolder, testCaseHref, type TestCaseLinkState } from "./jobPath";
import { buildTitle, notStartedTitle, relativeJob } from "./pageTitles";
import JobBreadcrumbs from "./JobBreadcrumbs";
import { ShortcutLegend } from "./ShortcutLegend";
import { useDocumentTitle } from "../../hooks/useDocumentTitle";
import { formatDuration } from "./JobTree";
import { breakablePath } from "./breakablePath";
import { groupTestCases } from "./testCaseGroups";
import { StageGraph } from "./StageGraph";
import { RunLogViewer, type RunLogViewerHandle } from "./RunLogViewer";
import { PendingInputCard } from "./PendingInputCard";
import { ReplayDrawer } from "./ReplayDrawer";
import styles from "./BuildPage.module.css";

/** The frames the run log stream carries. */
type RunLogFrame =
  | RunLogMetaEvent
  | RunLogStageEvent
  | RunLogLineEvent
  | RunLogGapEvent
  | RunLogLiveEvent
  | RunLogEndEvent;

type RunPageStatus =
  | "loading"
  | "found"
  | "forbidden"
  | "recordStoreOff"
  | "error"
  | "requested"
  | "notStarted"
  | "notFoundPolling"
  | "notFoundSettled";

const POLL_MS = 3000;
const REQUESTED_TIMEOUT_MS = 60_000;
const COLD_LOAD_TIMEOUT_MS = 30_000;

// A stable identity for a run with no stage list yet. StageGraph's reset
// effect keys on the stages prop's identity; a fresh array literal on
// every render would re-run that effect on every render and wipe the
// stages it merged from live "stage" events.
const EMPTY_STAGES: RunStage[] = [];

// One sentence per reason code the record store carries. The set is
// complete, and the fallback covers a code a newer recorder adds.
const TRIGGER_REASON_SENTENCES: Record<string, string> = {
  JOB_NOT_FOUND: "The job does not exist on the controller.",
  JOB_NOT_BUILDABLE: "The job cannot start a build.",
  TRIGGER_REJECTED: "The controller refused the trigger.",
  TRIGGER_UNREACHABLE: "The trigger did not reach the controller's Jenkins.",
  CLAIM_EXPIRED: "The build did not get a build number in time, so it did not start.",
  CLAIM_ABANDONED: "The build got a build number, but it did not start in time.",
  FENCED: "The controller that received the trigger no longer hosts this team.",
  CROSS_TENANT: "Another team may not start this job.",
  INVALID_KEY: "The build request was not valid.",
  THROTTLED: "The record store refused the build because of a limit.",
};
const TRIGGER_REASON_FALLBACK = "The controller refused the trigger.";

const REFUSED_COMMAND_LABELS: Record<RunCommandRefusal["command"], string> = {
  cancel: "Cancel",
  proceed_input: "Input proceed",
  abort_input: "Input abort",
};

/** The one-line text of a refused cancel or input answer. */
function describeCommandRefusal(r: RunCommandRefusal): string {
  let text = `${REFUSED_COMMAND_LABELS[r.command] ?? r.command} refused`;
  if (r.httpStatus) text += ` (HTTP ${r.httpStatus})`;
  if (r.actor) text += ` by ${r.actor}`;
  text += ` at ${new Date(r.refusedAt).toLocaleString()}`;
  return r.error ? `${text}: ${r.error}` : `${text}.`;
}

/**
 * Reads the run record, and polls it while the record can still change. A
 * record that does not exist yet keeps the pre-record poll under one of two
 * windows: a run this browser session just requested polls for 60 seconds
 * before it reports "not started", and a cold load of an unknown run polls
 * for 30 seconds and then settles on "not found". A record that exists
 * polls every 3 seconds only while the stream is not open and the run is
 * not terminal.
 */
function useRunRecord(runId: string | undefined, requested: boolean, streamOpen: boolean) {
  const [run, setRun] = useState<RunDetail | null>(null);
  const [status, setStatus] = useState<RunPageStatus>(requested ? "requested" : "loading");
  const startRef = useRef(Date.now());
  const settledRef = useRef(false);
  // The interval outlives the render that created it, so it reads the
  // record and the stream state through refs.
  const runRef = useRef<RunDetail | null>(null);
  const streamOpenRef = useRef(streamOpen);
  streamOpenRef.current = streamOpen;

  const fetchOnce = useCallback(async (): Promise<boolean> => {
    if (!runId) return true;
    try {
      const r = await getRun(runId);
      runRef.current = r;
      setRun(r);
      setStatus("found");
      return true;
    } catch (e) {
      if (e instanceof ApiError && e.status === 403) {
        setStatus("forbidden");
        return true;
      }
      if (e instanceof ApiError && e.status === 503) {
        setStatus("recordStoreOff");
        return true;
      }
      if (e instanceof ApiError && e.status === 404) return false;
      setStatus("error");
      return true;
    }
  }, [runId]);

  useEffect(() => {
    if (!runId) return;
    settledRef.current = false;
    runRef.current = null;
    startRef.current = Date.now();
    setRun(null);
    setStatus(requested ? "requested" : "loading");
    let cancelled = false;

    async function tick() {
      const done = await fetchOnce();
      if (cancelled) return;
      if (done) {
        settledRef.current = true;
        return;
      }
      const elapsed = Date.now() - startRef.current;
      if (requested) {
        setStatus(elapsed >= REQUESTED_TIMEOUT_MS ? "notStarted" : "requested");
      } else if (elapsed >= COLD_LOAD_TIMEOUT_MS) {
        setStatus("notFoundSettled");
        settledRef.current = true;
      } else {
        setStatus("notFoundPolling");
      }
    }

    void tick();
    const interval = setInterval(() => {
      // A record that does not exist yet keeps the pre-record poll, whatever
      // the stream state. A record that exists polls only while the stream
      // is not open and the run is not terminal.
      const current = runRef.current;
      if (settledRef.current && (!current || streamOpenRef.current || isTerminal(current))) return;
      void tick();
    }, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [runId, requested, fetchOnce]);

  const refetch = useCallback(() => void fetchOnce(), [fetchOnce]);
  return { run, status, refetch };
}

const RESULT_LABELS: Record<string, string> = {
  SUCCESS: "Passed",
  UNSTABLE: "Unstable",
  FAILURE: "Failed",
  ABORTED: "Aborted",
  INTERRUPTED: "Interrupted",
};
const RESULT_TONE: Record<string, "ok" | "bad" | "warn" | "idle"> = {
  SUCCESS: "ok",
  UNSTABLE: "warn",
  FAILURE: "bad",
  ABORTED: "idle",
  INTERRUPTED: "bad",
};

function describeRunState(run: RunDetail): { label: string; tone: "ok" | "bad" | "warn" | "idle" } {
  if (run.status !== "COMPLETED") {
    return run.status === "RUNNING"
      ? { label: "Running", tone: "idle" }
      : { label: "Queued", tone: "idle" };
  }
  if (run.result && RESULT_LABELS[run.result]) {
    return { label: RESULT_LABELS[run.result], tone: RESULT_TONE[run.result] };
  }
  return { label: run.result ?? "Completed", tone: "idle" };
}

function isTerminal(run: RunDetail): boolean {
  return run.status === "COMPLETED" || run.status === "DUPLICATE";
}

function isPaletteOpen(): boolean {
  return !!document.querySelector('[role="dialog"][aria-label="Global search"]');
}

type TabId = "log" | "artifacts" | "tests" | "changes";
const TAB_IDS = ["log", "artifacts", "tests", "changes"] as const;

function isTabId(v: string | null): v is TabId {
  return v !== null && (TAB_IDS as readonly string[]).includes(v);
}

export default function BuildPage() {
  const { team, job, run: runId } = useParams<{ team: string; job: string; run: string }>();
  const location = useLocation();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { toast } = useToast();
  const requested = !!(location.state as { requested?: boolean } | null)?.requested;

  // The team's stream carries every run of the team.
  // The notifier coalesces per run, so a lock frame for this run can stand
  // in for a stage frame. The page reads its own record on either frame.
  const { lastEvent: runEvent, readyState: runStreamReady } = useEventStream<RunNotification>(
    team ? teamRunStreamUrl(team) : null,
    team ? `pipelines:${team}` : null,
    ["run", "stage", "lock", "input"],
  );
  const streamOpen = runStreamReady === "open";
  const { run, status, refetch } = useRunRecord(runId, requested, streamOpen);

  const { data: permissions } = usePermissions();
  const folderRelativeJobPath = run ? stripTenantFolder(run.tenantFolder, run.jobPath) : undefined;
  const canWrite = run ? canAccessTenantPipeline(permissions, run.tenant, "write", folderRelativeJobPath) : false;

  // The log tab keeps every log frame, because the viewer unmounts while
  // another tab is active and one render can cover a whole burst of frames.
  // A ref holds the frames, and a counter tells the viewer how many of them
  // are new.
  const logFramesRef = useRef<SSEEvent<RunLogFrame>[]>([]);
  const [logVersion, setLogVersion] = useState(0);
  const [logEnded, setLogEnded] = useState(false);
  const logRunId = run?.runId;

  // The `end` frame is the log's last frame, so the page closes the stream on
  // it. A null URL keeps the stream closed, and the hook mints no further
  // ticket, so a finished log never reconnects.
  const logStreamUrl = logRunId && !logEnded ? runLogUrl(logRunId) : null;

  // The byte offset (and line number) of the last log-stream event this
  // browser read. A ref, not state: it must survive a reconnect without
  // itself triggering a render, and useEventStream reads it lazily through
  // resumeLogUrl only when a reconnect happens.
  const logWatermarkRef = useRef<{ from: number; fromLine?: number } | null>(null);
  const resumeLogUrl = useCallback((): string | null => {
    if (!logRunId) return null;
    const watermark = logWatermarkRef.current;
    return runLogUrl(logRunId, watermark ? { from: watermark.from, fromLine: watermark.fromLine } : undefined);
  }, [logRunId]);

  // The hook calls onEvent for every frame, including each frame of a burst
  // that reaches React as one render. The handler keeps the frame, advances
  // the watermark, and closes the stream on the closing frame.
  const onLogFrame = useCallback((frame: SSEEvent<RunLogFrame>) => {
    if (frame.type === "line") {
      const ev = frame.data as RunLogLineEvent;
      // The server resumes at the first line whose start offset is at or
      // after `from`. One byte past this line's start selects the next
      // line, so a reconnect never replays the line already rendered.
      logWatermarkRef.current = { from: ev.off + 1, fromLine: ev.n + 1 };
    }
    // A gap frame leaves the watermark alone. Its offset names lost bytes,
    // and it can lie before the last line or past the bytes the page read.
    // The line watermark already points at the first line not rendered.
    logFramesRef.current.push(frame);
    setLogVersion((v) => v + 1);
    if (frame.type === "end") setLogEnded(true);
  }, []);

  const { lastEvent } = useEventStream<RunLogFrame>(
    logStreamUrl,
    logRunId ? `run:${logRunId}` : null,
    ["meta", "stage", "line", "gap", "live", "end"],
    { resumeUrl: resumeLogUrl, onEvent: onLogFrame },
  );

  // A new run starts a new log, so the frames, the version, the end flag, and
  // the watermark of the run before it go.
  useEffect(() => {
    logFramesRef.current = [];
    logWatermarkRef.current = null;
    setLogVersion(0);
    setLogEnded(false);
  }, [logRunId]);

  // A stage frame, a run frame, a lock frame, and an input frame all change
  // the record, so the page refetches the record on any of them. The page
  // renders the record, never a payload field.
  useEffect(() => {
    if (!runEvent) return;
    if (
      runEvent.type !== "run" &&
      runEvent.type !== "stage" &&
      runEvent.type !== "lock" &&
      runEvent.type !== "input"
    ) {
      return;
    }
    const notification = runEvent.data;
    if (!notification || notification.run_id !== runId) return;
    void refetch();
  }, [runEvent, runId, refetch]);

  // The stream sends no first frame and replays nothing, so an open stream
  // is the page's signal to refetch the record.
  useEffect(() => {
    if (streamOpen) void refetch();
  }, [streamOpen, refetch]);

  const logViewerRef = useRef<RunLogViewerHandle>(null);
  // The query parameter is the single source of the active tab, so Back
  // changes the tab. A missing or unknown value shows the Log tab.
  const tabParam = searchParams.get("tab");
  const activeTab: TabId = isTabId(tabParam) ? tabParam : "log";
  const setActiveTab = useCallback(
    (id: TabId) =>
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev);
        next.set("tab", id);
        return next;
      }),
    [setSearchParams],
  );
  useEffect(() => {
    if (tabParam !== null && !isTabId(tabParam)) {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          next.delete("tab");
          return next;
        },
        { replace: true },
      );
    }
  }, [tabParam, setSearchParams]);
  const [legendOpen, setLegendOpen] = useState(false);
  const shortcutsButtonRef = useRef<HTMLButtonElement>(null);
  const closeLegend = useCallback(() => {
    setLegendOpen(false);
    shortcutsButtonRef.current?.focus();
  }, []);

  const artifactsQuery = useQuery({
    queryKey: ["run-artifacts", run?.runId],
    queryFn: () => listRunArtifacts(run!.runId),
    enabled: !!run,
  });
  const testsQuery = useQuery({
    queryKey: ["run-tests", run?.runId],
    queryFn: () => listRunTests(run!.runId),
    enabled: !!run,
  });
  const changesQuery = useQuery({
    queryKey: ["run-changes", run?.runId],
    queryFn: () => listRunChanges(run!.runId),
    enabled: !!run,
  });
  // The script read gates the Replay control. It runs only for a finished
  // run and only for a caller who holds the write grant. A failed read shows
  // no control and no reason.
  const scriptQuery = useQuery({
    queryKey: ["run-script", run?.runId],
    queryFn: () => getRunScript(run!.runId),
    enabled: !!run && isTerminal(run) && canWrite,
  });
  const [replayOpen, setReplayOpen] = useState(false);
  const neighborsQuery = useQuery({
    queryKey: ["run-neighbors", team, job],
    queryFn: () => listRuns(team!, job!, { limit: 100 }),
    enabled: !!team && !!job,
  });

  const cancelMutation = useMutation({
    mutationFn: () => cancelRun(run!.runId),
    onSuccess: () => {
      toast("Cancel requested");
      refetch();
    },
    onError: () => toast("Cancel failed"),
  });

  // The write routes answer 202 with a request, not an outcome, so each toast
  // says "requested". Only the refetched record clears an entry.
  const proceedInputMutation = useMutation({
    mutationFn: (v: { inputId: string; parameters: Record<string, string> }) =>
      proceedRunInput(run!.runId, v.inputId, v.parameters),
    onSuccess: () => {
      toast("Proceed requested");
      refetch();
    },
    onError: () => toast("Proceed failed"),
  });

  const abortInputMutation = useMutation({
    mutationFn: (inputId: string) => abortRunInput(run!.runId, inputId),
    onSuccess: () => {
      toast("Abort requested");
      refetch();
    },
    onError: () => toast("Abort failed"),
  });

  const rerunMutation = useMutation({
    mutationFn: () => rerunRun(run!.runId),
    onSuccess: (res) => {
      toast("Re-run requested");
      if (team && job) {
        navigate(
          `/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(job)}/${encodeURIComponent(res.runId)}`,
          { state: { requested: true } },
        );
      }
    },
    onError: () => toast("Re-run failed"),
  });

  const downloadArtifact = useCallback(
    async (path: string) => {
      if (!run) return;
      try {
        const blob = await downloadRunArtifact(run.runId, path);
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url;
        a.download = path.split("/").pop() || path;
        document.body.appendChild(a);
        a.click();
        a.remove();
        URL.revokeObjectURL(url);
      } catch {
        toast("Download failed");
      }
    },
    [run, toast],
  );

  // The neighbours are the builds next to this one in the newest-first list.
  // Outside that list, both are undefined and the controls stay disabled.
  const neighbors = useMemo(() => {
    const items = neighborsQuery.data?.items ?? [];
    const idx = run ? items.findIndex((r) => r.runId === run.runId) : -1;
    return idx === -1
      ? { older: undefined, newer: undefined }
      : { older: items[idx + 1], newer: items[idx - 1] };
  }, [neighborsQuery.data, run]);

  const openAdjacentBuild = useCallback(
    (direction: "older" | "newer") => {
      if (!run || !team || !job) return;
      const target = neighbors[direction];
      if (!target) return;
      const tabSuffix = isTabId(tabParam) ? `?tab=${tabParam}` : "";
      navigate(
        `/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(job)}/${encodeURIComponent(target.runId)}${tabSuffix}`,
      );
    },
    [run, team, job, neighbors, tabParam, navigate],
  );

  useEffect(() => {
    function handleKey(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      const target = e.target;
      if (target instanceof HTMLElement) {
        if (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable) return;
      }
      if (isPaletteOpen()) return;
      if (e.key === "?") {
        if (legendOpen) closeLegend();
        else setLegendOpen(true);
        return;
      }
      // While the list is open, no other key acts. Escape closes it.
      if (legendOpen) {
        if (e.key === "Escape") closeLegend();
        return;
      }
      switch (e.key) {
        case "j":
          openAdjacentBuild("older");
          break;
        case "k":
          openAdjacentBuild("newer");
          break;
        case "l":
          logViewerRef.current?.focusViewer();
          break;
        case "/":
          e.preventDefault();
          logViewerRef.current?.focusSearch();
          break;
        case "f":
          logViewerRef.current?.toggleFollow();
          break;
        case "t":
          logViewerRef.current?.toggleTimestamps();
          break;
        default:
          return;
      }
    }
    document.addEventListener("keydown", handleKey);
    return () => document.removeEventListener("keydown", handleKey);
  }, [openAdjacentBuild, legendOpen, closeLegend]);

  const totalsRow = useMemo(
    () => testsQuery.data?.items.find((i) => i.suite === "_total"),
    [testsQuery.data],
  );
  const suiteRows = useMemo(
    () => testsQuery.data?.items.filter((i) => i.suite !== "_total") ?? [],
    [testsQuery.data],
  );
  // The response carries the failed cases first and the skipped cases
  // after. The tab keeps that order and sorts no row.
  const caseRows = testsQuery.data?.cases ?? [];
  const caseGroups = useMemo(() => groupTestCases(caseRows), [caseRows]);
  const testReport = testsQuery.data?.report;

  const docTitle =
    !run || !team
      ? null
      : run.result === "TRIGGER_FAILED"
        ? notStartedTitle(team, run.tenantFolder, run.jobPath)
        : buildTitle(team, run, describeRunState(run).label);
  useDocumentTitle(docTitle);

  if (status === "loading") {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading build...</div>
      </div>
    );
  }
  if (status === "forbidden") return <ForbiddenPage />;
  if (status === "recordStoreOff") {
    return (
      <RecoveryState
        kind="error"
        title="Pipeline history is off"
        message="This Varroa install has no build record store configured, so build history is unavailable."
        onRetry={refetch}
      />
    );
  }
  if (status === "error") return <GenericErrorPage onRetry={refetch} />;
  if (status === "notFoundPolling" || status === "notFoundSettled") {
    return (
      <div className={styles.page}>
        <div className={styles.notFound}>
          <h1>Run not found</h1>
          <p>This run may still be starting.</p>
        </div>
      </div>
    );
  }
  if (status === "requested") {
    return (
      <div className={styles.page}>
        <div className={styles.pending}>
          <h1>Requested</h1>
          <p>Waiting for the build to start...</p>
        </div>
      </div>
    );
  }
  if (status === "notStarted") {
    return (
      <div className={styles.page}>
        <div className={styles.pending}>
          <h1>Not started</h1>
          <p>The reason is not available.</p>
        </div>
      </div>
    );
  }
  if (!run) return <GenericErrorPage onRetry={refetch} />;

  // A refused trigger started no build, so the page shows a terminal card
  // instead of the graph, the duration, and the build number. The card
  // renders here, not inside the requested or the not-started block: a
  // record now exists, so neither block runs.
  if (run.result === "TRIGGER_FAILED") {
    return (
      <div className={styles.page}>
        {team && (
          <JobBreadcrumbs team={team} tenantFolder={run.tenantFolder} jobPath={run.jobPath} current="Not started" />
        )}
        <div className={styles.triggerFailed} role="alert">
          <h1>Build not started</h1>
          <p>{TRIGGER_REASON_SENTENCES[run.triggerReasonCode ?? ""] ?? TRIGGER_REASON_FALLBACK}</p>
          {run.triggerError && <p className={styles.mono}>{run.triggerError}</p>}
          <p>Re-run starts with each parameter&apos;s declared default.</p>
        </div>
        {canWrite && (
          <div className={styles.actions}>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => rerunMutation.mutate()}
              disabled={!!run.retryRunId || rerunMutation.isPending}
            >
              <RotateCcw size={14} aria-hidden="true" /> Re-run
            </Button>
          </div>
        )}
      </div>
    );
  }

  const desc = describeRunState(run);
  const interrupted = run.result === "INTERRUPTED";
  const rerunDisabled = !!run.retryRunId || !isTerminal(run);
  const cancelable = !isTerminal(run);
  const replayable = canWrite && isTerminal(run) && scriptQuery.data?.replayable === true;

  const jobLeaf = relativeJob(run.tenantFolder, run.jobPath).split("/").pop();

  const tabs = [
    { id: "log", label: "Log" },
    { id: "artifacts", label: `Artifacts${artifactsQuery.data ? ` (${artifactsQuery.data.items.length})` : ""}` },
    { id: "tests", label: `Tests${totalsRow ? ` (${totalsRow.total})` : ""}` },
    { id: "changes", label: `Changes${changesQuery.data ? ` (${changesQuery.data.items.length})` : ""}` },
  ];

  return (
    <div className={styles.page}>
      <div className={styles.head}>
        {team && (
          <JobBreadcrumbs
            team={team}
            tenantFolder={run.tenantFolder}
            jobPath={run.jobPath}
            current={`#${run.buildNumber ?? "?"}`}
          />
        )}
        <div className={styles.headTop}>
          <h1 className={styles.title}>
            {jobLeaf} · Build #{run.buildNumber ?? "?"}
          </h1>
          <span className={`${styles.statusPill} ${styles[`tone_${desc.tone}`]}`}>{desc.label}</span>
          <nav aria-label="Build navigation" className={styles.buildNav}>
            <Button
              variant="ghost"
              size="sm"
              aria-keyshortcuts="j"
              onClick={() => openAdjacentBuild("older")}
              disabled={!neighbors.older}
            >
              <ChevronLeft size={14} aria-hidden="true" /> Older
            </Button>
            <Button
              variant="ghost"
              size="sm"
              aria-keyshortcuts="k"
              onClick={() => openAdjacentBuild("newer")}
              disabled={!neighbors.newer}
            >
              Newer <ChevronRight size={14} aria-hidden="true" />
            </Button>
            <span className={styles.shortcutsAnchor}>
              <Button
                ref={shortcutsButtonRef}
                variant="ghost"
                size="sm"
                aria-keyshortcuts="?"
                aria-expanded={legendOpen}
                onClick={() => (legendOpen ? closeLegend() : setLegendOpen(true))}
              >
                <Keyboard size={14} aria-hidden="true" /> Shortcuts
              </Button>
              <ShortcutLegend open={legendOpen} onClose={closeLegend} />
            </span>
          </nav>
        </div>
        <div className={styles.meta}>
          {run.branch && <span>{run.branch}</span>}
          {run.scmRevision && <span className={styles.mono}>{run.scmRevision.slice(0, 8)}</span>}
          {run.actor && <span>{run.actor}</span>}
          {run.cause && <span>{run.cause}</span>}
          {run.startedAt && <span>{new Date(run.startedAt).toLocaleString()}</span>}
          {typeof run.durationMs === "number" && <span>{formatDuration(run.durationMs)}</span>}
        </div>
        {/* The summary row renders the run's own summary line. It hides
            itself when the record carries no summary. */}
        {run.summary && (
          <div className={styles.ranWith}>
            <span className={styles.ranWithLabel}>Summary</span>
            <span>{run.summary}</span>
          </div>
        )}
        {/* The row lists how the run was defined. It hides itself when the
            record carries no definition path, no library, and no pod template. */}
        {run.source &&
          !!(run.source.definitionPath || run.source.libraries?.length || run.source.podTemplates?.length) && (
            <div className={styles.ranWith}>
              <span className={styles.ranWithLabel}>Ran with</span>
              {run.source.definitionPath && <span className={styles.mono}>{run.source.definitionPath}</span>}
              {(run.source.libraries ?? []).map((lib, i) => (
                <span key={`lib-${i}-${lib.name}`}>
                  {lib.version ? `${lib.name}@${lib.version}` : lib.name}
                  {lib.revision ? ` ${lib.revision.slice(0, 8)}` : ""}
                </span>
              ))}
              {(run.source.podTemplates ?? []).map((pod, i) => (
                <span key={`pod-${i}-${pod.name}`}>{pod.image ? `${pod.name} (${pod.image})` : pod.name}</span>
              ))}
            </div>
          )}
        {canWrite && (
          <div className={styles.actions}>
            {cancelable && (
              <Button variant="ghost" size="sm" onClick={() => cancelMutation.mutate()} disabled={cancelMutation.isPending}>
                <XCircle size={14} aria-hidden="true" /> Cancel
              </Button>
            )}
            <Button
              variant="ghost"
              size="sm"
              onClick={() => rerunMutation.mutate()}
              disabled={rerunDisabled || rerunMutation.isPending}
            >
              <RotateCcw size={14} aria-hidden="true" /> Re-run
            </Button>
            {replayable && (
              <Button variant="ghost" size="sm" onClick={() => setReplayOpen(true)}>
                <Repeat size={14} aria-hidden="true" /> Replay
              </Button>
            )}
          </div>
        )}
      </div>

      {/* A missing start time is the never-started signal. A real run can
          hold a null build number after a number conflict, but every real
          run holds a start time. */}
      {interrupted && (
        <div className={styles.interruptedBanner} role="alert">
          <span>Interrupted{run.interruptedReason ? `: ${run.interruptedReason}` : ": reason not available"}.</span>
          {!run.startedAt && <span> The build never started on the cell.</span>}
          {run.retryRunId && team && job && (
            <a
              href={`/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(job)}/${encodeURIComponent(run.retryRunId)}`}
            >
              View retry
            </a>
          )}
          {!run.startedAt && <p>Re-run starts with each parameter&apos;s declared default.</p>}
        </div>
      )}

      {/* The notice names the newest cancel or input answer Jenkins
          refused. A later successful command does not remove it, so the
          notice states its own time. */}
      {run.commandRefusal && (
        <div className={styles.refusalNotice} role="status">
          {describeCommandRefusal(run.commandRefusal)}
        </div>
      )}

      {/* The digest card sits above the Pipeline card and under the
          interrupted banner. Each part renders on its own, so a digest that
          carries one part shows that part alone. */}
      {run.digest && (
        <Card title="Digest">
          {run.digest.stage && <p><strong>{run.digest.stage}</strong>{run.digest.error ? `: ${run.digest.error}` : ""}</p>}
          {run.digest.failedTests && (
            <p>{run.digest.failedTests.failed} failed{typeof run.digest.failedTests.skipped === "number" ? `, ${run.digest.failedTests.skipped} skipped` : ""}{typeof run.digest.failedTests.total === "number" ? ` of ${run.digest.failedTests.total}` : ""}</p>
          )}
          {/* The list names the digest's own failed cases. It holds at most
              ten entries, and it is absent when the digest names no case. */}
          {!!run.digest.failedTests?.cases?.length && team && job && (
            <ul className={styles.digestCases} aria-label="Failed test cases">
              {run.digest.failedTests.cases.map((c) => (
                <li key={`${c.suite}/${c.caseKey}`}>
                  <Link
                    to={testCaseHref(team, job, c.caseKey)}
                    state={{ className: c.className, name: c.name } satisfies TestCaseLinkState}
                    className={styles.mono}
                  >
                    {c.className} › {c.name}
                  </Link>
                </li>
              ))}
            </ul>
          )}
          {run.digest.logTail && <pre className={`${styles.mono} ${styles.digestTail}`}>{run.digest.logTail}</pre>}
        </Card>
      )}

      {/* A pending input sits under the digest card and above the Pipeline
          card. The write gate is the page's own, so a caller who cannot
          write sees the answer controls gone. */}
      {!!run.inputs?.length && (
        <PendingInputCard
          inputs={run.inputs}
          canWrite={canWrite}
          busyInputId={
            proceedInputMutation.isPending
              ? proceedInputMutation.variables?.inputId
              : abortInputMutation.isPending
                ? abortInputMutation.variables
                : null
          }
          onProceed={(inputId, parameters) => proceedInputMutation.mutate({ inputId, parameters })}
          onAbort={(inputId) => abortInputMutation.mutate(inputId)}
        />
      )}

      <Card title="Pipeline">
        <StageGraph
          stages={run.stages ?? EMPTY_STAGES}
          lastEvent={lastEvent}
          terminal={isTerminal(run)}
          onSelectStage={(stageId) => logViewerRef.current?.scrollToStage(stageId)}
        />
      </Card>

      <Tabs
        tabs={tabs}
        activeTab={activeTab}
        onSelect={(id) => setActiveTab(id as TabId)}
        idBase="run"
        ariaLabel="Run output"
      />

      <div {...tabPanelProps("run", activeTab)}>
      {activeTab === "log" && (
        <RunLogViewer
          key={run.runId}
          ref={logViewerRef}
          runId={run.runId}
          frames={logFramesRef.current}
          version={logVersion}
          runTerminal={isTerminal(run)}
          logFinalized={run.logFinalized === true}
          stages={run.stages ?? EMPTY_STAGES}
        />
      )}

      {activeTab === "artifacts" && (
        <div className={styles.tableScroll}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Path</th>
                <th>Size</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {(artifactsQuery.data?.items ?? []).map((a) => (
                <tr key={a.path}>
                  <td>{breakablePath(a.path)}</td>
                  <td>{a.sizeBytes} bytes</td>
                  <td>
                    <button
                      type="button"
                      disabled={!a.downloadable}
                      title={a.downloadable ? undefined : "Stored locally by Jenkins; not downloadable"}
                      onClick={() => downloadArtifact(a.path)}
                    >
                      Download
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {activeTab === "tests" && (
        <div className={styles.testsTab}>
          <p className={styles.testsNotice}>
            The case table shows the failed and skipped cases the run reported, at most 500. The
            test report holds every case, including each passed case.
          </p>
          {testReport && (
            <div className={styles.reportLine}>
              <span className={styles.ranWithLabel}>Test report</span>
              <span className={styles.mono}>{testReport.path}</span>
              <span>{testReport.sizeBytes} bytes</span>
              <button
                type="button"
                disabled={!testReport.downloadable}
                title={testReport.downloadable ? undefined : "Stored locally by Jenkins; not downloadable"}
                onClick={() => downloadArtifact(testReport.path)}
              >
                Download
              </button>
            </div>
          )}
          {caseRows.length > 0 && team && job && (
            <div className={styles.tableScroll}>
              <table className={styles.table} aria-label="Test cases">
                <thead>
                  <tr>
                    {caseGroups.showClassColumn && <th>Class</th>}
                    <th>Test</th>
                    <th>Status</th>
                    <th>Duration</th>
                    <th>Error details</th>
                  </tr>
                </thead>
                {caseGroups.groups.map((group, index) => (
                  <tbody key={`${group.suite}/${index}`}>
                    <tr className={styles.groupRow}>
                      <th scope="rowgroup" colSpan={caseGroups.showClassColumn ? 5 : 4}>
                        {breakablePath(group.label)}
                        {group.secondary !== undefined && (
                          <span className={styles.groupSecondary}>{breakablePath(group.secondary)}</span>
                        )}
                      </th>
                    </tr>
                    {group.rows.map((c) => (
                      <tr key={`${c.suite}/${c.caseKey}`}>
                        {caseGroups.showClassColumn && (
                          <td className={styles.mono}>{breakablePath(c.className)}</td>
                        )}
                        <td>
                          <Link
                            to={testCaseHref(team, job, c.caseKey)}
                            state={{ className: c.className, name: c.name } satisfies TestCaseLinkState}
                          >
                            {breakablePath(c.name)}
                          </Link>
                        </td>
                        <td>{c.status}</td>
                        <td>{typeof c.durationMs === "number" ? formatDuration(c.durationMs) : null}</td>
                        <td>
                          {c.errorDetails ? <pre className={`${styles.mono} ${styles.caseError}`}>{c.errorDetails}</pre> : null}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                ))}
              </table>
            </div>
          )}
          <div className={styles.tableScroll}>
            <table className={styles.table} aria-label="Test suites">
              <thead>
                <tr>
                  <th>Suite</th>
                  <th>Total</th>
                  <th>Failed</th>
                  <th>Skipped</th>
                </tr>
              </thead>
              <tbody>
                {suiteRows.map((s) => (
                  <tr key={s.suite}>
                    <td>{breakablePath(s.suite)}</td>
                    <td>{s.total}</td>
                    <td>{s.failed}</td>
                    <td>{s.skipped}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {activeTab === "changes" && (
        <ul className={styles.changesList}>
          {(changesQuery.data?.items ?? []).map((c, i) => (
            <li key={i}>{JSON.stringify(c)}</li>
          ))}
        </ul>
      )}
      </div>

      {/* The replay response reports no outcome. The page opens the new
          run in the requested state, the path the re-run control takes. */}
      {replayOpen && replayable && scriptQuery.data && (
        <ReplayDrawer
          runId={run.runId}
          script={scriptQuery.data.script}
          loadedScripts={scriptQuery.data.loadedScripts ?? {}}
          onClose={() => setReplayOpen(false)}
          onSubmitted={(newRunId) => {
            setReplayOpen(false);
            if (team && job) {
              navigate(
                `/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(job)}/${encodeURIComponent(newRunId)}`,
                { state: { requested: true } },
              );
            }
          }}
        />
      )}
    </div>
  );
}
