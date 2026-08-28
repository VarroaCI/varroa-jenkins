import { useParams, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { getBroodSchedule, listBroodOperations } from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import styles from "./BroodScheduleDetail.module.css";

export default function BroodScheduleDetail() {
  const { namespace, name } = useParams<{ namespace: string; name: string }>();
  const navigate = useNavigate();
  const ns = namespace ?? "";
  const schedName = name ?? "";

  const { data: sched, isLoading, error } = useQuery({
    queryKey: ["brood-schedule", ns, schedName],
    queryFn: () => getBroodSchedule(ns, schedName),
    enabled: !!ns && !!schedName,
  });

  const { data: runHistory } = useQuery({
    queryKey: ["brood-operations", "startedBy", ns, schedName],
    queryFn: () => listBroodOperations(undefined, undefined, `schedule:${ns}/${schedName}`),
    enabled: !!ns && !!schedName,
  });

  if (isLoading) return <p>Loading…</p>;
  if (error || !sched) return <p>Failed to load brood schedule.</p>;

  const spec = sched.spec;
  const status = sched.status;
  const historyItems = runHistory?.items ?? [];

  return (
    <div className={styles.page}>
      <header className={styles.header}>
        <h1>{ns}/{schedName}</h1>
        <div className={styles.actions}>
          <Button onClick={() => navigate("/brood-schedules")}>Back</Button>
        </div>
      </header>

      {/* Tenancy violation banner */}
      {status?.reason && (
        <div className={styles.warningBanner}>
          ⚠ Status: {status.reason}
        </div>
      )}

      {/* Schedule config */}
      <section className={styles.section}>
        <h2>Configuration</h2>
        <div className={styles.meta}>
          <div><strong>Schedule:</strong> {spec.schedule}</div>
          <div><strong>Suspended:</strong> {spec.suspend ? "Yes" : "No"}</div>
          <div><strong>Concurrency policy:</strong> {spec.concurrencyPolicy ?? "Forbid (default)"}</div>
          <div><strong>Wait for completion:</strong> {spec.waitForCompletion ? "Yes" : "No"}</div>
          <div><strong>Verb:</strong> {spec.template.action.verb}</div>
          <div><strong>Targets:</strong> {spec.template.targets.names?.join(", ") ?? "(selector)"}</div>
          <div><strong>Clusters:</strong> {spec.template.clusters?.join(", ") ?? "—"}</div>
          {spec.template.execution?.order && <div><strong>Order:</strong> {spec.template.execution.order}</div>}
          {spec.template.execution?.maxParallel && <div><strong>Max parallel:</strong> {spec.template.execution.maxParallel}</div>}
          {spec.template.execution?.failurePolicy && <div><strong>Failure policy:</strong> {spec.template.execution.failurePolicy}</div>}
          {spec.template.ttlSecondsAfterFinished && <div><strong>TTL:</strong> {spec.template.ttlSecondsAfterFinished}s</div>}
        </div>
      </section>

      {/* Status fields */}
      <section className={styles.section}>
        <h2>Status</h2>
        <div className={styles.meta}>
          <div><strong>Last schedule time:</strong> {status?.lastScheduleTime ? new Date(status.lastScheduleTime).toLocaleString() : "—"}</div>
          <div>
            <strong>Last successful time:</strong> {status?.lastSuccessfulTime ? new Date(status.lastSuccessfulTime).toLocaleString() : "—"}
            {status?.lastSuccessfulTime && (
              <span style={{ fontSize: "0.85rem", color: "var(--text-2)", display: "block", marginTop: "0.25rem" }}>
                {spec.waitForCompletion
                  ? "↳ The watch observed no failure (not necessarily run success)."
                  : "↳ The trigger job completed (not the run outcome)."}
              </span>
            )}
          </div>
          <div><strong>Active jobs:</strong> {status?.active?.length ?? 0}</div>
          <div><strong>Reason:</strong> {status?.reason ?? "—"}</div>
        </div>
      </section>

      {/* Run history */}
      <section className={styles.section}>
        <h2>Run History</h2>
        {historyItems.length === 0 && <p>No runs yet.</p>}
        {historyItems.length > 0 && (
          <Card>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Verb</th>
                  <th>Phase</th>
                  <th>Summary</th>
                </tr>
              </thead>
              <tbody>
                {historyItems.map((row) => (
                  <tr
                    key={row.namespace + "/" + row.name}
                    onClick={() => navigate(`/brood-operations/${encodeURIComponent(row.namespace)}/${encodeURIComponent(row.name)}`)}
                    style={{ cursor: "pointer" }}
                  >
                    <td className={styles.nameCell}>{row.namespace}/{row.name}</td>
                    <td>{row.verb}</td>
                    <td>{row.phase}</td>
                    <td>{row.summary.total}/{row.summary.succeeded}/{row.summary.failed}/{row.summary.skipped}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Card>
        )}
      </section>
    </div>
  );
}
