import { useState, useCallback, useEffect } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  listBroodOperations,
  deleteBroodOperation,
  suspendBroodOperation,
} from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { BroodControllerPicker } from "../components/BroodControllerPicker";
import { BroodOperationModal } from "../components/BroodOperationModal";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere } from "../hooks/usePermissions";
import type { BroodRunSummaryRow, BroodRunClusterStatus } from "../types";
import styles from "./BroodOperations.module.css";

const PHASE_ORDER: Record<string, number> = {
  Running: 0,
  Suspended: 1,
  Pending: 2,
  Succeeded: 3,
  Failed: 4,
  Canceled: 5,
};

function age(timestamp?: string): string {
  if (!timestamp) return "—";
  const d = Date.now() - new Date(timestamp).getTime();
  const s = Math.floor(d / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

export default function BroodOperations() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { permissions } = useAuth();
  const canManage = canDoAnywhere(permissions, "controllers", "manage");
  const [suspendTarget, setSuspendTarget] = useState<{ ns: string; name: string; suspend: boolean } | null>(null);
  const [cancelTarget, setCancelTarget] = useState<{ ns: string; name: string } | null>(null);
  const [runModalOpen, setRunModalOpen] = useState(false);
  const [runTargets, setRunTargets] = useState<string[]>([]);

  const openRunModal = () => {
    setRunTargets([]);
    setRunModalOpen(true);
  };

  const closeRunModal = () => {
    setRunModalOpen(false);
    setRunTargets([]);
  };

  const handleRunSubmitted = () => {
    closeRunModal();
    queryClient.invalidateQueries({ queryKey: ["brood-operations"] });
  };

  useEffect(() => {
    if (!runModalOpen) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setRunModalOpen(false);
        setRunTargets([]);
      }
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [runModalOpen]);

  const { data, isLoading, error } = useQuery({
    queryKey: ["brood-operations"],
    queryFn: () => listBroodOperations(),
    refetchInterval: 10_000,
  });

  const items: BroodRunSummaryRow[] = data?.items ?? [];
  const clusterStatuses: BroodRunClusterStatus[] = data?.clusters ?? [];

  const handleCancel = useCallback(async (ns: string, name: string) => {
    try {
      const result = await deleteBroodOperation(name, ns);
      // Show toast for any failed clusters
      const failures = result.clusters.filter((c) => !c.ok);
      if (failures.length > 0) {
        console.warn("cancel partial failure:", failures);
      }
      queryClient.invalidateQueries({ queryKey: ["brood-operations"] });
    } catch (e) {
      console.error("cancel failed", e);
    }
    setCancelTarget(null);
  }, [queryClient]);

  const handleSuspend = useCallback(async (ns: string, name: string, suspend: boolean) => {
    try {
      const result = await suspendBroodOperation(name, ns, suspend);
      const failures = result.clusters.filter((c) => !c.ok);
      if (failures.length > 0) {
        console.warn("suspend partial failure:", failures);
      }
      queryClient.invalidateQueries({ queryKey: ["brood-operations"] });
    } catch (e) {
      console.error("suspend failed", e);
    }
    setSuspendTarget(null);
  }, [queryClient]);

  const sorted = [...items].sort((a, b) => {
    const pa = PHASE_ORDER[a.phase] ?? 99;
    const pb = PHASE_ORDER[b.phase] ?? 99;
    if (pa !== pb) return pa - pb;
    return (a.createdAt ?? "").localeCompare(b.createdAt ?? "");
  });

  // Check for degraded clusters in the fan-out
  const hasDegraded = clusterStatuses.some((s) => !s.ok);

  return (
    <div className={styles.page}>
      <header className={styles.header}>
				<div><h1>Brood Operations</h1><p className={styles.subtitle}>Review active and completed changes across controllers</p></div>
        <div className={styles.headerActions}>
          {canManage && <Button variant="primary" onClick={openRunModal}>Run operation…</Button>}
          <Button onClick={() => navigate("/controllers")}>Back to Controllers</Button>
        </div>
      </header>

      {isLoading && <p>Loading…</p>}
			{error && <p className={styles.error}>Failed to load Brood Operations history</p>}

      {hasDegraded && (
        <div className={styles.degradedBanner}>
          Some clusters are unreachable. Data may be incomplete.
        </div>
      )}

      {!isLoading && sorted.length === 0 && (
        <div className={styles.empty}>
					<p>No Brood Operations yet.</p>
          <p>
            {canManage
              ? 'Click "Run operation…" above, or select controllers on the Controllers page.'
              : 'Ask someone with manage permission to select controllers on the Controllers page and choose "Run operation…".'}
          </p>
        </div>
      )}

      {sorted.length > 0 && (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
								<th>Operation</th>
								<th>Target scope</th>
								<th>Progress</th>
								<th>Outcome</th>
								<th>Timing</th>
                {canManage && <th>Actions</th>}
              </tr>
            </thead>
            <tbody>
              {sorted.map((row) => (
                <BroodOpRow
                  key={row.namespace + "/" + row.name}
                  row={row}
                  canManage={canManage}
                  onCancel={(ns, name) => setCancelTarget({ ns, name })}
                  onSuspend={(ns, name, suspend) => setSuspendTarget({ ns, name, suspend })}
                  onClick={(ns, name) => navigate(`/brood-operations/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`)}
                />
              ))}
            </tbody>
          </table>
        </Card>
      )}

      {/* Cancel confirmation */}
      {cancelTarget && (
        <div className={styles.modal} onClick={() => setCancelTarget(null)}>
          <div className={styles.modalContent} onClick={(e) => e.stopPropagation()}>
            <p>Cancel {cancelTarget.ns}/{cancelTarget.name}?</p>
            <div className={styles.modalActions}>
              <Button onClick={() => setCancelTarget(null)}>No</Button>
              <Button variant="primary" onClick={() => handleCancel(cancelTarget.ns, cancelTarget.name)}>Yes, Cancel</Button>
            </div>
          </div>
        </div>
      )}

      {suspendTarget && (
        <div className={styles.modal} onClick={() => setSuspendTarget(null)}>
          <div className={styles.modalContent} onClick={(e) => e.stopPropagation()}>
            <p>{suspendTarget.suspend ? "Suspend" : "Resume"} {suspendTarget.ns}/{suspendTarget.name}?</p>
            <div className={styles.modalActions}>
              <Button onClick={() => setSuspendTarget(null)}>No</Button>
              <Button variant="primary" onClick={() => handleSuspend(suspendTarget.ns, suspendTarget.name, suspendTarget.suspend)}>
                {suspendTarget.suspend ? "Suspend" : "Resume"}
              </Button>
            </div>
          </div>
        </div>
      )}

      {runModalOpen && (
        <div className={styles.runOverlay} onClick={closeRunModal}>
          <div
            className={styles.runDialog}
            role="dialog"
            aria-modal="true"
            aria-labelledby="run-brood-operation-title"
            onClick={(e) => e.stopPropagation()}
          >
            <div id="run-brood-operation-title" className={styles.runDialogTitle}>Run Brood Operation</div>
            <div className={styles.runStepLabel}>1. Select controllers</div>
            <BroodControllerPicker selected={runTargets} onSelectionChange={setRunTargets} compact />
            {runTargets.length > 0 && (
              <>
                <div className={styles.runStepLabel}>2. Configure &amp; run</div>
                <BroodOperationModal
                  targets={runTargets}
                  embedded
                  onClose={closeRunModal}
                  onSubmitted={handleRunSubmitted}
                />
              </>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function BroodOpRow({ row, canManage, onCancel, onSuspend, onClick }: {
  row: BroodRunSummaryRow;
  canManage: boolean;
  onCancel: (ns: string, name: string) => void;
  onSuspend: (ns: string, name: string, suspend: boolean) => void;
  onClick: (ns: string, name: string) => void;
}) {
  const ns = row.namespace;
  const name = row.name;
  const verb = row.verb;
  const phase = row.phase;
  const summary = row.summary;
  const isTerminal = phase === "Succeeded" || phase === "Failed" || phase === "Canceled";
  const isSuspended = phase === "Suspended";

  return (
    <tr onClick={() => onClick(ns, name)} style={{ cursor: "pointer" }}>
			<td className={styles.nameCell}><strong>{ns}/{name}</strong><span>{verb}</span></td>
			<td><strong>{row.clusters.length} cluster{row.clusters.length === 1 ? "" : "s"}</strong><span>{row.clusters.join(", ") || "No reachable clusters"}</span></td>
			<td><PhaseBadge phase={phase} /><span>{summary.succeeded + summary.failed + summary.skipped}/{summary.total} complete</span></td>
			<td className={summary.failed > 0 ? styles.failure : undefined}>{summary.succeeded} succeeded · {summary.failed} failed · {summary.skipped} skipped{summary.failed > 0 && <span>Open details for failure context</span>}</td>
			<td>{age(row.createdAt)} ago<span>{row.createdAt ? new Date(row.createdAt).toLocaleString() : "Created time unavailable"}</span></td>
      {canManage && (
        <td>
          <div className={styles.actionButtons}>
            {!isTerminal && (
              <>
                <Button size="sm" onClick={(e) => { e.stopPropagation(); onSuspend(ns, name, !isSuspended); }}>
                  {isSuspended ? "Resume" : "Suspend"}
                </Button>
                <Button size="sm" variant="ghost" onClick={(e) => { e.stopPropagation(); onCancel(ns, name); }}>
                  Cancel
                </Button>
              </>
            )}
          </div>
        </td>
      )}
    </tr>
  );
}

const PHASE_CLASS: Record<string, string> = {
  Running: styles.phaseRunning,
  Suspended: styles.phaseSuspended,
  Pending: styles.phasePending,
  Succeeded: styles.phaseSucceeded,
  Failed: styles.phaseFailed,
  Canceled: styles.phaseCanceled,
};

function PhaseBadge({ phase }: { phase: string }) {
  return (
    <span className={`${styles.phasePill} ${PHASE_CLASS[phase] ?? styles.phasePending}`}>
      <span className={styles.phaseDot} />
      {phase}
    </span>
  );
}
