import { Fragment, useCallback } from "react";
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
  const { lastEvent } = useEventStream<BroodRun>(streamUrl, streamScope);

  const run: BroodRun | undefined = lastEvent?.data ?? initial;

  const isTerminal = run?.phase === "Succeeded" || run?.phase === "Failed" || run?.phase === "Canceled";
  const isSuspended = run?.phase === "Suspended";

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
