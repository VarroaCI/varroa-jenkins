import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Button } from "../../components/Button";
import { listPipelines } from "../../api/client";
import { canAccessTenantPipeline } from "../../hooks/usePermissions";
import type { Permissions } from "../../types/auth";
import { stripTenantFolder } from "../pipelines/jobPath";
import styles from "./RunPipelineDialog.module.css";

/** RunPipelineDialogProps names the tenants the caller may write. */
export interface RunPipelineDialogProps {
  tenants: string[];
  permissions: Permissions | undefined;
  onClose: () => void;
}

const enc = (s: string) => encodeURIComponent(s);

/**
 * RunPipelineDialog picks a pipeline job and opens its job page with run=1.
 * The job page then opens its own run drawer, so the run form lives in one
 * place. The dialog offers only the tenants the caller may write, and only
 * the jobs of kind pipeline. With one writable tenant, that tenant is
 * preselected.
 */
export function RunPipelineDialog({ tenants, permissions, onClose }: RunPipelineDialogProps) {
  const navigate = useNavigate();
  const [tenant, setTenant] = useState(tenants.length === 1 ? tenants[0] : "");
  const [job, setJob] = useState("");

  const jobs = useQuery({
    queryKey: ["pipelines-run-dialog-jobs", tenant],
    queryFn: () => listPipelines(tenant),
    enabled: tenant !== "",
  });
  // A job-pattern grant can exclude a job, so each job passes the same
  // folder-relative check the job page applies before the run form opens.
  const pipelineJobs = (jobs.data?.items ?? []).filter(
    (item) =>
      item.kind === "pipeline" &&
      canAccessTenantPipeline(
        permissions,
        tenant,
        "write",
        stripTenantFolder(jobs.data?.tenantFolder ?? "", item.jobPath),
      ),
  );

  // Escape closes the dialog, as every other dialog of the app does.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  const chooseTenant = (next: string) => {
    setTenant(next);
    setJob("");
  };

  let jobNote: string | null = null;
  if (tenant !== "" && jobs.isError) jobNote = "Varroa could not load the jobs.";
  else if (tenant !== "" && jobs.isSuccess && pipelineJobs.length === 0) jobNote = "This tenant has no pipeline job.";

  return (
    <div
      className={styles.overlay}
      onMouseDown={(e) => {
        // Only a press that starts on the backdrop closes the dialog.
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className={styles.dialog} role="dialog" aria-modal="true" aria-labelledby="run-pipeline-title">
        <h2 id="run-pipeline-title" className={styles.dialogTitle}>
          Run a pipeline
        </h2>
        <label className={styles.field}>
          <span>Tenant</span>
          <select
            aria-label="Tenant"
            value={tenant}
            onChange={(e) => chooseTenant(e.target.value)}
            autoFocus
          >
            <option value="">Choose a tenant</option>
            {tenants.map((name) => (
              <option key={name} value={name}>
                {name}
              </option>
            ))}
          </select>
        </label>
        <label className={styles.field}>
          <span>Job</span>
          <select
            aria-label="Job"
            value={job}
            onChange={(e) => setJob(e.target.value)}
            disabled={tenant === "" || !jobs.isSuccess || pipelineJobs.length === 0}
          >
            <option value="">{tenant === "" ? "Choose a tenant first" : jobs.isLoading ? "Loading jobs…" : "Choose a job"}</option>
            {pipelineJobs.map((item) => (
              <option key={item.jobPath} value={item.jobPath}>
                {stripTenantFolder(jobs.data?.tenantFolder ?? "", item.jobPath) || item.jobPath}
              </option>
            ))}
          </select>
        </label>
        {jobNote && (
          <p className={styles.refusal} role="alert">
            {jobNote}
            {jobs.isError && (
              <>
                {" "}
                <button type="button" className={styles.retry} onClick={() => void jobs.refetch()}>
                  Retry
                </button>
              </>
            )}
          </p>
        )}
        <div className={styles.dialogActions}>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={tenant === "" || job === ""}
            onClick={() => {
              navigate(`/pipelines/${enc(tenant)}/${enc(job)}?run=1`);
              onClose();
            }}
          >
            Continue
          </Button>
        </div>
      </div>
    </div>
  );
}
