// ApprovalRow shows one approval: the tool, its target, its arguments, and
// the decision. Approve is disabled with the reason when the viewer may not
// decide it. The server checks every decision again.
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { approveAssistantApproval, denyAssistantApproval } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { labelFor } from "./labels";
import { usePermissions } from "../../hooks/usePermissions";
import { Button } from "../Button";
import { useAssistant } from "./AssistantProvider";
import { approveBlockReason, canDeny, type ApprovalRun } from "./approvalHint";
import type { ApprovalView } from "./transcript";
import styles from "./ApprovalRow.module.css";

export interface ApprovalRowProps {
  approval: ApprovalView;
  viewer: string;
  principal: string;
  run?: ApprovalRun;
  onResolved?: () => void;
}

const REASON_MAX = 500;

/** ApprovalRow renders one approval and its decision controls. */
export function ApprovalRow({ approval: a, viewer, principal, run, onResolved }: ApprovalRowProps) {
  const assistant = useAssistant();
  const queryClient = useQueryClient();
  const perms = usePermissions();
  const [busy, setBusy] = useState(false);
  const [denying, setDenying] = useState(false);
  const [reason, setReason] = useState("");
  const [error, setError] = useState("");

  const blocked = approveBlockReason(a, viewer, principal, perms.data, assistant.paused, run);
  const denyOK = canDeny(a, viewer, principal);

  const decide = async (approve: boolean) => {
    setBusy(true);
    setError("");
    try {
      if (approve) await approveAssistantApproval(a.id);
      else await denyAssistantApproval(a.id, { reason: reason.trim() || undefined });
      assistant.resolved(a.sessionId);
      void queryClient.invalidateQueries({ queryKey: ["assistant-approvals"] });
      onResolved?.();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        void queryClient.invalidateQueries({ queryKey: ["assistant-session", a.sessionId] });
        void queryClient.invalidateQueries({ queryKey: ["assistant-approvals"] });
        if (err.code === "kill_switch") assistant.resolved(a.sessionId);
        if (err.code === "approval_expired") setError(labelFor("errorCodes", "approval_expired"));
        else setError("This approval changed. Reload and try again.");
      } else {
        setError("The decision was not recorded.");
      }
    } finally {
      setBusy(false);
    }
  };

  const resolvedText =
    a.state === "approved" ? `Approved by ${a.resolvedBy ?? "someone"}`
    : a.state === "denied" ? (a.resolvedBy ? `Denied by ${a.resolvedBy}` : "Denied")
    : a.state === "expired" ? "Expired"
    : null;

  return (
    <div className={styles.row}>
      <div className={styles.head}>
        <strong>{a.tool}</strong>
        {a.targetRef && <span className={styles.target}>{a.targetKind} {a.targetRef}</span>}
        {resolvedText ? <span>{resolvedText}</span> : null}
      </div>
      <pre className={styles.args}>{JSON.stringify(a.arguments, null, 2)}</pre>
      {(() => {
        const args = a.arguments as { files?: { path: string; content: string }[] } | null;
        if (!args?.files || !Array.isArray(args.files)) return null;
        return args.files.map((f) => (
          <div key={f.path}>
            <h4 className={styles.path}>{f.path}</h4>
            <pre className={styles.file}>{f.content}</pre>
          </div>
        ));
      })()}
      {a.state === "pending" && (
        <div className={styles.actions}>
          <Button
            size="sm"
            disabled={busy || blocked !== null}
            title={blocked ?? undefined}
            onClick={() => void decide(true)}
          >
            Approve
          </Button>
          {denyOK && !denying && (
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => setDenying(true)}>Deny</Button>
          )}
          {denying && (
            <form
              className={styles.reasonForm}
              onSubmit={(e) => {
                e.preventDefault();
                void decide(false);
              }}
            >
              <input
                aria-label="Reason"
                maxLength={REASON_MAX}
                value={reason}
                onChange={(e) => setReason(e.target.value)}
              />
              <Button type="submit" size="sm" disabled={busy}>Confirm deny</Button>
            </form>
          )}
        </div>
      )}
      {error && <p role="alert" className={styles.error}>{error}</p>}
    </div>
  );
}
