// approvalHint says why a viewer may not approve or deny an approval. The
// server checks every decision again. The hint only spares a click that the
// server would refuse.
import type { Permissions } from "../../types/auth";
import { canAccessTenantPipeline, canDoInNamespace } from "../../hooks/usePermissions";
import type { ApprovalView } from "./transcript";
import { stripTenantFolder } from "../../pages/pipelines/jobPath";

export interface ApprovalRun {
  tenant: string;
  tenantFolder: string;
  jobPath: string;
}

/** approveBlockReason returns why approval is blocked, or null when it is allowed. */
export function approveBlockReason(
  a: ApprovalView,
  viewer: string,
  principal: string,
  perms: Permissions | undefined,
  paused: boolean,
  run?: ApprovalRun,
  now: number = Date.now(),
): string | null {
  if (a.state !== "pending") return "This approval is already resolved.";
  if (Date.parse(a.expiresAt) <= now) return "This approval has expired.";
  if (paused) return "The assistant is paused.";
  if (viewer !== principal) return "Only the person who started this conversation can approve it.";
  switch (a.targetKind) {
    case "run": {
      if (!run) return "Loading the run.";
      const job = stripTenantFolder(run.tenantFolder, run.jobPath);
      return canAccessTenantPipeline(perms, run.tenant, "write", job)
        ? null
        : "You cannot write to this build's pipeline.";
    }
    case "job": {
      const slash = a.targetRef.indexOf("/");
      const tenant = slash < 0 ? a.targetRef : a.targetRef.slice(0, slash);
      const job = slash < 0 ? "" : a.targetRef.slice(slash + 1);
      return canAccessTenantPipeline(perms, tenant, "write", job)
        ? null
        : "You cannot write to this job's pipeline.";
    }
    case "controller": {
      const parts = a.targetRef.split("/");
      const ns = parts[1] ?? "";
      return canDoInNamespace(perms, ns, "controllers", "update")
        ? null
        : "You cannot update this controller.";
    }
    default:
      return null;
  }
}

/** canDeny reports whether the viewer may deny the approval. */
export function canDeny(a: ApprovalView, viewer: string, principal: string, now: number = Date.now()): boolean {
  return a.state === "pending" && Date.parse(a.expiresAt) > now && viewer === principal;
}
