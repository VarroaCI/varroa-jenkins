import { Fragment, useCallback, useEffect, useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  getBroodOperation,
  deleteBroodOperation,
  suspendBroodOperation,
  broodStreamUrl,
} from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere } from "../hooks/usePermissions";
import { useEventStream } from "../hooks/useEventStream";
import type { BroodRun, BroodRunCluster, BroodTargetStatus } from "../types";
import styles from "./BroodOperationDetail.module.css";

const STATE_CLASS: Record<string, string> = {
  Pending: styles.statePending,
  Dispatched: styles.stateDispatched,
  Succeeded: styles.stateSucceeded,
  Failed: styles.stateFailed,
  Skipped: styles.stateSkipped,
};

function StateBadge({ state }: { state: string }) {
  return (
    <span className={`${styles.statePill} ${STATE_CLASS[state] ?? styles.statePending}`}>
      <span className={styles.stateDot} />
      {state}
    </span>
  );
}

export default function BroodOperationDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { permissions } = useAuth();
  const canManage = canDoAnywhere(permissions, "controllers", "manage");
  const ns = namespace ?? "";
  const opName = name ?? "";

  const { data: initial, isLoading, error } = useQuery({
    queryKey: ["brood-operation", ns, opName],
    queryFn: () => getBroodOperation(opName, ns),
    enabled: !!ns && !!opName,
  });

  const streamUrl = ns && opName ? broodStreamUrl(opName, ns) : null;
  const streamScope = ns && opName ? `broodop:${ns}/${opName}` : null;
  // The query may already have loaded a completed operation (Succeeded /
  // Failed / Canceled): there are no live updates to follow, so don't even
  // open the stream in that case.
  const initialTerminal =
    initial?.phase === "Succeeded" || initial?.phase === "Failed" || initial?.phase === "Canceled";
  // Once the operation reaches a terminal phase, no more updates are coming —
  // stop the stream for good by handing useEventStream a null baseUrl. This is
  // scoped to the current ns/opName pair: navigating between operation detail
  // pages reuses this component instance (the route has no `key`), so a
  // previous operation's terminal state must not leave the next operation's
  // stream closed.
  const [stopStream, setStopStream] = useState(false);
  // The latest live `status` frame's BroodRun. A `closed` frame's payload is
  // never assigned here, so it can never clobber this — see the effect below
  // for why we don't otherwise need to inspect `closed` frames at all.
  const [lastStatus, setLastStatus] = useState<BroodRun | undefined>(undefined);
  const { lastEvent } = useEventStream<BroodRun>(
    stopStream || initialTerminal ? null : streamUrl,
    streamScope,
    ["status", "closed"],
  );

  // On navigation (ns/opName change) reset per-operation stream state so the
  // new operation gets a fresh stream instead of inheriting the previous
  // operation's terminal state.
  useEffect(() => {
    setStopStream(false);
    setLastStatus(undefined);
  }, [ns, opName]);

  // Only `status` frames carry a BroodRun payload. Deliberately NOT reacting
  // to `closed` frames here: the BFF (internal/api/handlers_broodops.go,
  // handleBroodStreamWithPoll) always sends a terminal `status` frame
  // immediately before any genuinely-terminal `closed` frame, so a terminal
  // `status` is a fully sufficient signal on its own. The only `closed`
  // frames that arrive WITHOUT a preceding terminal status are the ambiguous
  // ones (the one-hour `deadline_exceeded` backstop, or a transient
  // `BroodOps.Get` error) where the operation may still be running — for
  // those we deliberately do nothing special and let useEventStream's
  // existing backoff-and-reconnect (mint a fresh ticket on error, already
  // reviewed clean) pick the stream back up on its own, instead of freezing
  // the page on a one-shot refetch.
  useEffect(() => {
    if (lastEvent?.type === "status") {
      setLastStatus(lastEvent.data);
    }
  }, [lastEvent]);

  // Prefer the latest live status over the page-load snapshot, falling back
  // to `initial` before the stream has delivered anything. This is safe
  // without any precedence-flip: `lastStatus` only ever holds `status`
  // payloads (never a `closed` frame's bare `{}`), and an operation's phase
  // never regresses from terminal back to non-terminal, so once `lastStatus`
  // reports a phase it's never stale in a way `initial` could correct.
  const run: BroodRun | undefined = lastStatus ?? initial;

  const isTerminal = run?.phase === "Succeeded" || run?.phase === "Failed" || run?.phase === "Canceled";
  const isSuspended = run?.phase === "Suspended";

  useEffect(() => {
    if (!stopStream && isTerminal) {
      setStopStream(true);
    }
  }, [stopStream, isTerminal]);

  const handleCancel = useCallback(async () => {
    if (!ns || !opName) return;
    try {
      await deleteBroodOperation(opName, ns);
      queryClient.invalidateQueries({ queryKey: ["brood-operations"] });
      navigate("/brood-operations");
    } catch (e) {
      console.error("cancel failed", e);
    }
  }, [ns, opName, queryClient, navigate]);

  const handleSuspend = useCallback(async () => {
    if (!ns || !opName) return;
    try {
      await suspendBroodOperation(opName, ns, true);
      queryClient.invalidateQueries({ queryKey: ["brood-operation", ns, opName] });
    } catch (e) {
      console.error("suspend failed", e);
    }
  }, [ns, opName, queryClient]);

  const handleResume = useCallback(async () => {
    if (!ns || !opName) return;
    try {
      await suspendBroodOperation(opName, ns, false);
      queryClient.invalidateQueries({ queryKey: ["brood-operation", ns, opName] });
    } catch (e) {
      console.error("resume failed", e);
    }
  }, [ns, opName, queryClient]);

  if (isLoading) return <p>Loading…</p>;
  if (error || !run) return <p>Failed to load brood operation.</p>;

  const summary = run.summary;

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h1>{ns}/{opName}</h1>
        <div className={styles.actions}>
          <Button onClick={() => navigate("/brood-operations")}>Back</Button>
          {canManage && !isTerminal && (
            <>
              <Button onClick={isSuspended ? handleResume : handleSuspend}>
                {isSuspended ? "Resume" : "Suspend"}
              </Button>
              <Button variant="ghost" onClick={handleCancel}>Cancel</Button>
            </>
          )}
        </div>
      </header>

      <div className={styles.meta}>
        <div><strong>Verb:</strong> {run.verb}</div>
        <div><strong>Phase:</strong> {run.phase}</div>
        {run.startedBy && <div><strong>Started by:</strong> {run.startedBy}</div>}
        {run.createdAt && <div><strong>Created at:</strong> {new Date(run.createdAt).toLocaleString()}</div>}
        <div>
          <strong>Summary:</strong> {summary.total} total, {summary.succeeded} succeeded, {summary.failed} failed, {summary.skipped} skipped
        </div>
      </div>

      {/* Per-cluster sections */}
      {run.clusters.map((cluster, idx) => (
        <ClusterSection key={idx} cluster={cluster} />
      ))}
    </div>
  );
}

const OPERATION_REASON_TEXT: Record<string, string> = {
  TargetVersionUnresolved: "The target version or line for this upgrade could not be resolved against any version profile.",
};

function ClusterSection({ cluster }: { cluster: BroodRunCluster }) {
  const targets: BroodTargetStatus[] = cluster.op?.status?.targets ?? [];

  return (
    <section className={styles.targetsSection}>
      <h2>Cluster: {cluster.cluster}</h2>
      {!cluster.ok && (
        <p className={styles.error}>Unreachable: {cluster.error ?? "unknown error"}</p>
      )}
      {cluster.ok && cluster.op && (
        <>
          <div className={`${styles.meta} ${styles.subMeta}`}>
            <div><strong>Phase:</strong> {cluster.op.status.phase ?? "—"}</div>
            <div><strong>Started by:</strong> {cluster.op.status.startedBy ?? "—"}</div>
            {cluster.op.status.reason && (
              <div>
                <strong>Reason:</strong>{" "}
                {OPERATION_REASON_TEXT[cluster.op.status.reason] ?? cluster.op.status.reason}
                {OPERATION_REASON_TEXT[cluster.op.status.reason] && (
                  <span className={styles.subMeta}> ({cluster.op.status.reason})</span>
                )}
              </div>
            )}
          </div>
          {targets.length === 0 && <p>No targets resolved yet.</p>}
          {targets.length > 0 && (
            <Card>
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th>Namespace</th>
                    <th>Name</th>
                    <th>Wave</th>
                    <th>State</th>
                    <th>Reason</th>
                  </tr>
                </thead>
                <tbody>
                  {targets.map((t) => (
                    <Fragment key={`${t.namespace}/${t.name}`}>
                      <tr>
                        <td>{t.namespace}</td>
                        <td>{t.name}</td>
                        <td>{t.wave}</td>
                        <td><StateBadge state={t.state} /></td>
                        <td>{t.reason ?? "—"}</td>
                      </tr>
                      {t.output && (
                        <tr className={styles.outputRow}>
                          <td colSpan={5}>
                            <details>
                              <summary className={styles.outputSummary}>Output</summary>
                              <pre className={styles.outputPre}>{t.output}</pre>
                            </details>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  ))}
                </tbody>
              </table>
            </Card>
          )}
        </>
      )}
    </section>
  );
}
