import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";
import { RotateCcw, XCircle } from "lucide-react";
import {
  cancelRun,
  getRun,
  listRunArtifacts,
  listRunChanges,
  listRuns,
  listRunTests,
  rerunRun,
  runLogUrl,
  downloadRunArtifact,
} from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { useEventStream } from "../../hooks/useEventStream";
import { canAccessTenantPipeline, usePermissions } from "../../hooks/usePermissions";
import { useToast } from "../../components/Toast";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import { Tabs } from "../../components/Tabs";
import { ForbiddenPage, GenericErrorPage, RecoveryState } from "../../components/RecoveryState";
import type {
  RunDetail,
  RunLogEndEvent,
  RunLogGapEvent,
  RunLogLineEvent,
  RunLogLiveEvent,
  RunLogMetaEvent,
  RunLogStageEvent,
  RunStage,
} from "../../types";
import { stripTenantFolder } from "./jobPath";
import { StageGraph } from "./StageGraph";
import { RunLogViewer, type RunLogViewerHandle } from "./RunLogViewer";
import styles from "./BuildPage.module.css";

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
};
const TRIGGER_REASON_FALLBACK = "The controller refused the trigger.";

/**
 * Reads the run record, and — while no record exists yet — polls it under
 * one of two windows. A run this browser session just requested polls for
 * 60 seconds before it reports "not started"; polling never stops, so a
 * late record still resolves the page. A cold load of an unknown run polls
 * for 30 seconds and then settles on "not found", because the page cannot
 * otherwise tell a starting run from an unknown one.
 */
function useRunRecord(runId: string | undefined, requested: boolean) {
  const [run, setRun] = useState<RunDetail | null>(null);
  const [status, setStatus] = useState<RunPageStatus>(requested ? "requested" : "loading");
  const startRef = useRef(Date.now());
  const settledRef = useRef(false);

  const fetchOnce = useCallback(async (): Promise<boolean> => {
    if (!runId) return true;
    try {
      const r = await getRun(runId);
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
      if (!settledRef.current) void tick();
    }, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [runId, requested, fetchOnce]);

  return { run, status, refetch: () => void fetchOnce() };
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

export default function BuildPage() {
  const { team, job, run: runId } = useParams<{ team: string; job: string; run: string }>();
  const location = useLocation();
  const navigate = useNavigate();
  const { toast } = useToast();
  const requested = !!(location.state as { requested?: boolean } | null)?.requested;
  const { run, status, refetch } = useRunRecord(runId, requested);

  const { data: permissions } = usePermissions();
  const folderRelativeJobPath = run ? stripTenantFolder(run.tenantFolder, run.jobPath) : undefined;
  const canWrite = run ? canAccessTenantPipeline(permissions, run.tenant, "write", folderRelativeJobPath) : false;

  const logStreamUrl = run ? runLogUrl(run.runId) : null;

  // The byte offset (and line number) of the last log-stream event this
  // browser read. A ref, not state: it must survive a reconnect without
  // itself triggering a render, and useEventStream reads it lazily through
  // resumeLogUrl only when a reconnect happens.
  const logWatermarkRef = useRef<{ from: number; fromLine?: number } | null>(null);
  const resumeLogUrl = useCallback((): string | null => {
    if (!run) return null;
    const watermark = logWatermarkRef.current;
    return runLogUrl(run.runId, watermark ? { from: watermark.from, fromLine: watermark.fromLine } : undefined);
  }, [run]);

  const { lastEvent } = useEventStream<
    RunLogMetaEvent | RunLogStageEvent | RunLogLineEvent | RunLogGapEvent | RunLogLiveEvent | RunLogEndEvent
  >(logStreamUrl, run ? `run:${run.runId}` : null, ["meta", "stage", "line", "gap", "live", "end"], {
    resumeUrl: resumeLogUrl,
  });

  // Track the watermark from the stream itself so a reconnect resumes past
  // what was already read instead of replaying the log from byte zero. A
  // `gap` event still names an offset the client has effectively consumed
  // (the bytes are unrecoverable), so it advances the watermark too.
  useEffect(() => {
    if (!lastEvent) return;
    if (lastEvent.type === "line") {
      const ev = lastEvent.data as RunLogLineEvent;
      // The server resumes at the first line whose start offset is at or
      // after `from`. One byte past this line's start selects the next
      // line, so a reconnect never replays the line already rendered.
      logWatermarkRef.current = { from: ev.off + 1, fromLine: ev.n + 1 };
    } else if (lastEvent.type === "gap") {
      const ev = lastEvent.data as RunLogGapEvent;
      logWatermarkRef.current = { from: ev.off };
    }
  }, [lastEvent]);

  const logViewerRef = useRef<RunLogViewerHandle>(null);
  const [activeTab, setActiveTab] = useState<TabId>("log");

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

  const openAdjacentBuild = useCallback(
    (direction: "older" | "newer") => {
      if (!run || !team || !job) return;
      const items = neighborsQuery.data?.items ?? [];
      const idx = items.findIndex((r) => r.runId === run.runId);
      if (idx === -1) return;
      const target = items[direction === "older" ? idx + 1 : idx - 1];
      if (!target) return;
      navigate(`/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(job)}/${encodeURIComponent(target.runId)}`);
    },
    [run, team, job, neighborsQuery.data, navigate],
  );

  useEffect(() => {
    function handleKey(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      const target = e.target;
      if (target instanceof HTMLElement) {
        if (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable) return;
      }
      if (isPaletteOpen()) return;
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
  }, [openAdjacentBuild]);

  const totalsRow = useMemo(
    () => testsQuery.data?.items.find((i) => i.suite === "_total"),
    [testsQuery.data],
  );
  const suiteRows = useMemo(
    () => testsQuery.data?.items.filter((i) => i.suite !== "_total") ?? [],
    [testsQuery.data],
  );

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

  const tabs = [
    { id: "log", label: "Log" },
    { id: "artifacts", label: `Artifacts${artifactsQuery.data ? ` (${artifactsQuery.data.items.length})` : ""}` },
    { id: "tests", label: `Tests${totalsRow ? ` (${totalsRow.total})` : ""}` },
    { id: "changes", label: `Changes${changesQuery.data ? ` (${changesQuery.data.items.length})` : ""}` },
  ];

  return (
    <div className={styles.page}>
      <div className={styles.head}>
        <div className={styles.headTop}>
          <h1 className={styles.title}>
            Build #{run.buildNumber ?? "?"}
            {run.cause ? <span className={styles.commitMessage}> — {run.cause}</span> : null}
          </h1>
          <span className={`${styles.statusPill} ${styles[`tone_${desc.tone}`]}`}>{desc.label}</span>
        </div>
        <div className={styles.meta}>
          {run.branch && <span>{run.branch}</span>}
          {run.scmRevision && <span className={styles.mono}>{run.scmRevision.slice(0, 8)}</span>}
          {run.actor && <span>{run.actor}</span>}
          {run.startedAt && <span>{new Date(run.startedAt).toLocaleString()}</span>}
          {typeof run.durationMs === "number" && <span>{Math.round(run.durationMs / 1000)}s</span>}
        </div>
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
          </div>
        )}
      </div>

      {interrupted && (
        <div className={styles.interruptedBanner} role="alert">
          <span>Interrupted{run.interruptedReason ? `: ${run.interruptedReason}` : ": reason not available"}.</span>
          {run.retryRunId && team && job && (
            <a
              href={`/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(job)}/${encodeURIComponent(run.retryRunId)}`}
            >
              View retry
            </a>
          )}
        </div>
      )}

      <Card title="Pipeline">
        <StageGraph
          stages={run.stages ?? EMPTY_STAGES}
          lastEvent={lastEvent}
          terminal={isTerminal(run)}
          onSelectStage={(stageId) => logViewerRef.current?.scrollToStage(stageId)}
        />
      </Card>

      <Tabs tabs={tabs} activeTab={activeTab} onSelect={(id) => setActiveTab(id as TabId)} />

      {activeTab === "log" && (
        <RunLogViewer ref={logViewerRef} runId={run.runId} lastEvent={lastEvent} runTerminal={isTerminal(run)} />
      )}

      {activeTab === "artifacts" && (
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
                <td>{a.path}</td>
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
      )}

      {activeTab === "tests" && (
        <div>
          <p className={styles.testsNotice}>Individual test cases are not stored.</p>
          <table className={styles.table}>
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
                  <td>{s.suite}</td>
                  <td>{s.total}</td>
                  <td>{s.failed}</td>
                  <td>{s.skipped}</td>
                </tr>
              ))}
            </tbody>
          </table>
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
  );
}
