import { useState, useCallback } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  listBroodSchedules,
  createBroodSchedule,
  deleteBroodSchedule,
  suspendBroodSchedule,
} from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { BroodControllerPicker } from "../components/BroodControllerPicker";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere } from "../hooks/usePermissions";
import { broodTargetShape } from "../lib/broodTargets";
import { parseClearableInt, clampClearableInt } from "../lib/numericInput";
import type { BroodSchedule, CreateBroodScheduleRequest, BroodVerb, BroodOrder, BroodFailurePolicy } from "../types";
import styles from "./BroodSchedules.module.css";

export default function BroodSchedules() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { permissions } = useAuth();
  const canManage = canDoAnywhere(permissions, "controllers", "manage");
  const [showCreate, setShowCreate] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<{ ns: string; name: string } | null>(null);
  const [suspendTarget, setSuspendTarget] = useState<{ ns: string; name: string; suspend: boolean } | null>(null);

  const { data: items, isLoading, error } = useQuery({
    queryKey: ["brood-schedules"],
    queryFn: () => listBroodSchedules(),
    refetchInterval: 10_000,
  });

  const schedules: BroodSchedule[] = items?.items ?? [];

  const handleDelete = useCallback(async (ns: string, name: string) => {
    try {
      await deleteBroodSchedule(ns, name);
      queryClient.invalidateQueries({ queryKey: ["brood-schedules"] });
    } catch (e) {
      console.error("delete failed", e);
    }
    setDeleteTarget(null);
  }, [queryClient]);

  const handleSuspend = useCallback(async (ns: string, name: string, suspend: boolean) => {
    try {
      await suspendBroodSchedule(ns, name, suspend);
      queryClient.invalidateQueries({ queryKey: ["brood-schedules"] });
    } catch (e) {
      console.error("suspend failed", e);
    }
    setSuspendTarget(null);
  }, [queryClient]);

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h1>Brood Schedules</h1>
        <div className={styles.headerActions}>
          {canManage && <Button variant="primary" onClick={() => setShowCreate(true)}>Create schedule…</Button>}
          <Button onClick={() => navigate("/controllers")}>Back to Controllers</Button>
        </div>
      </header>

      {isLoading && <p>Loading…</p>}
      {error && <p className={styles.error}>Failed to load brood schedules</p>}

      {!isLoading && schedules.length === 0 && (
        <div className={styles.empty}>
          <p>No brood schedules yet.</p>
          <p>{canManage ? 'Click "Create schedule…" above to create one.' : "Ask someone with manage permission to create a schedule."}</p>
        </div>
      )}

      {schedules.length > 0 && (
        <Card>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Name</th>
                <th>Schedule</th>
                <th>Suspended</th>
                <th>Last Schedule</th>
                <th>Wait</th>
                <th>Cluster</th>
                {canManage && <th>Actions</th>}
              </tr>
            </thead>
            <tbody>
              {schedules.map((s) => (
                <tr
                  key={s.namespace + "/" + s.name}
                  onClick={() => navigate(`/brood-schedules/${encodeURIComponent(s.namespace)}/${encodeURIComponent(s.name)}`)}
                  style={{ cursor: "pointer" }}
                >
                  <td className={styles.nameCell}>{s.namespace}/{s.name}</td>
                  <td>{s.spec.schedule}</td>
                  <td>{s.spec.suspend ? "Yes" : "No"}</td>
                  <td>{s.status?.lastScheduleTime ? new Date(s.status.lastScheduleTime).toLocaleString() : "—"}</td>
                  <td>{s.spec.waitForCompletion ? "Yes" : "No"}</td>
                  <td>{s.cluster ?? "—"}</td>
                  {canManage && (
                    <td>
                      <div className={styles.actionButtons}>
                        <Button size="sm" onClick={(e) => { e.stopPropagation(); setSuspendTarget({ ns: s.namespace, name: s.name, suspend: !s.spec.suspend }); }}>
                          {s.spec.suspend ? "Resume" : "Suspend"}
                        </Button>
                        <Button size="sm" variant="ghost" onClick={(e) => { e.stopPropagation(); setDeleteTarget({ ns: s.namespace, name: s.name }); }}>
                          Delete
                        </Button>
                      </div>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}

      {/* Delete confirmation */}
      {deleteTarget && (
        <div className={styles.modal} onClick={() => setDeleteTarget(null)}>
          <div className={styles.modalContent} onClick={(e) => e.stopPropagation()}>
            <p>Delete {deleteTarget.ns}/{deleteTarget.name}?</p>
            <div className={styles.modalActions}>
              <Button onClick={() => setDeleteTarget(null)}>No</Button>
              <Button variant="primary" onClick={() => handleDelete(deleteTarget.ns, deleteTarget.name)}>Yes, Delete</Button>
            </div>
          </div>
        </div>
      )}

      {/* Suspend/Resume confirmation */}
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

      {/* Create dialog */}
      {showCreate && (
        <CreateScheduleDialog
          onClose={() => setShowCreate(false)}
          onCreated={() => {
            setShowCreate(false);
            queryClient.invalidateQueries({ queryKey: ["brood-schedules"] });
          }}
        />
      )}
    </div>
  );
}

function CreateScheduleDialog({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const [name, setName] = useState("");
  const [cron, setCron] = useState("");
  const [verb, setVerb] = useState<BroodVerb>("reconcile");
  const [waitForCompletion, setWaitForCompletion] = useState(true);
  const [concurrencyPolicy, setConcurrencyPolicy] = useState("");
  const [ttl, setTtl] = useState<number | "">(0);
  const [maxParallel, setMaxParallel] = useState<number | "">(1);
  const [order, setOrder] = useState<BroodOrder>("rolloutWave");
  const [failurePolicy, setFailurePolicy] = useState<BroodFailurePolicy>("FailTidy");
  const [targets, setTargets] = useState<string[]>([]);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");

  const handleCreate = async () => {
    setError("");
    if (!name.trim()) { setError("Name is required"); return; }
    if (!cron.trim()) { setError("Cron expression is required"); return; }
    if (targets.length === 0) { setError("Select at least one controller"); return; }

    setCreating(true);
    try {
      // Shape the "cluster/ns/name" picker keys into the tenancy form the
      // brood API expects (explicit namespace + bare names, or qualified
      // names). This is what keeps the request out of the operator-namespace
      // mode that rejects bare, unqualified names with a 400.
      const shape = broodTargetShape(targets);
      const req: CreateBroodScheduleRequest = {
        name: name.trim(),
        namespace: shape.namespace,
        spec: {
          schedule: cron.trim(),
          waitForCompletion,
          template: {
            targets: { names: shape.names },
            action: { verb },
            clusters: shape.clusters,
            execution: {
              maxParallel: maxParallel !== "" && maxParallel > 1 ? maxParallel : undefined,
              order,
              failurePolicy,
            },
            ttlSecondsAfterFinished: ttl !== "" && ttl > 0 ? ttl : undefined,
          },
        },
      };
      if (concurrencyPolicy) {
        req.spec.concurrencyPolicy = concurrencyPolicy;
      }
      await createBroodSchedule(req);
      onCreated();
    } catch (e: any) {
      setError(e.message ?? "Create failed");
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className={styles.modal} onClick={() => { if (!creating) onClose(); }}>
      <div
        className={styles.dialog}
        role="dialog"
        aria-modal="true"
        aria-label="Create Brood Schedule"
        onClick={(e) => e.stopPropagation()}
      >
        <div className={styles.dialogTitle}>Create Brood Schedule</div>

        <div className={styles.formGrid}>
          <label>Name:
            <input type="text" value={name} onChange={(e) => setName(e.target.value)} />
          </label>
          <label>Cron:
            <input type="text" value={cron} onChange={(e) => setCron(e.target.value)} placeholder="*/5 * * * *" />
          </label>
          <label>Verb:
            <select value={verb} onChange={(e) => setVerb(e.target.value as BroodVerb)}>
              <option value="restart">restart</option>
              <option value="reprovision">reprovision</option>
              <option value="reconcile">reconcile</option>
              <option value="stop">stop</option>
              <option value="start">start</option>
            </select>
          </label>
          <label>Concurrency policy:
            <select value={concurrencyPolicy} onChange={(e) => setConcurrencyPolicy(e.target.value)}>
              <option value="">(default)</option>
              <option value="Allow">Allow</option>
              <option value="Forbid">Forbid</option>
              <option value="Replace">Replace</option>
            </select>
          </label>
          <label>Max parallel:
            <input
              type="number"
              min={1}
              value={maxParallel}
              onChange={(e) => setMaxParallel(parseClearableInt(e.target.value))}
              onBlur={() => setMaxParallel((v) => clampClearableInt(v, 1))}
            />
          </label>
          <label>Order:
            <select value={order} onChange={(e) => setOrder(e.target.value as BroodOrder)}>
              <option value="rolloutWave">rolloutWave</option>
              <option value="name">name</option>
            </select>
          </label>
          <label>Failure policy:
            <select value={failurePolicy} onChange={(e) => setFailurePolicy(e.target.value as BroodFailurePolicy)}>
              <option value="FailFast">FailFast</option>
              <option value="FailTidy">FailTidy</option>
              <option value="FailAtEnd">FailAtEnd</option>
            </select>
          </label>
          <label>TTL seconds:
            <input
              type="number"
              min={0}
              value={ttl}
              onChange={(e) => setTtl(parseClearableInt(e.target.value))}
              onBlur={() => setTtl((v) => clampClearableInt(v, 0))}
            />
          </label>
          <label className={styles.checkboxRow}>
            <input type="checkbox" checked={waitForCompletion} onChange={(e) => setWaitForCompletion(e.target.checked)} />
            Wait for completion
          </label>
        </div>

        <div className={styles.pickerSection}>
          <div className={styles.pickerLabel}>Target controllers</div>
          <BroodControllerPicker selected={targets} onSelectionChange={setTargets} compact />
        </div>

        {error && <p className={styles.errorText}>{error}</p>}

        <div className={styles.dialogActions}>
          <Button onClick={onClose} disabled={creating}>Cancel</Button>
          <Button variant="primary" onClick={handleCreate} disabled={creating}>
            {creating ? "Creating…" : "Create"}
          </Button>
        </div>
      </div>
    </div>
  );
}
