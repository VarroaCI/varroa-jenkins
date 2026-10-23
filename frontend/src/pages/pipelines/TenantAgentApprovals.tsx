// TenantAgentApprovals lists the pending approvals of the Tenant agent sessions.
// Approve and Deny show only to a viewer who holds the tenant write check.
// The server checks every decision again.
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { approveAssistantApproval, denyAssistantApproval, listTenantAgentSessions } from "../../api/client";
import { usePermissions, canAccessTenantPipeline } from "../../hooks/usePermissions";
import { ApiError } from "../../hooks/useApi";
import { labelFor } from "../../components/assistant/labels";
import { Card } from "../../components/Card";
import type { AssistantApproval } from "../../types/assistant";
import styles from "./TenantAgentPage.module.css";

/** TenantAgentApprovals renders the waiting approvals of one Tenant agent. */
export function TenantAgentApprovals({ tenant }: { tenant: string }) {
  const queryClient = useQueryClient();
  const perms = usePermissions().data;
  const canWrite = canAccessTenantPipeline(perms, tenant, "write");
  const [refusal, setRefusal] = useState<string | null>(null);
  const [reasons, setReasons] = useState<Record<string, string>>({});
  const list = useQuery({
    queryKey: ["tenant-agent-sessions", tenant, "waiting_approval"],
    queryFn: () => listTenantAgentSessions(tenant, { state: "waiting_approval" }),
  });

  const pending: { sessionId: string; approval: AssistantApproval }[] = [];
  for (const s of list.data?.items ?? []) {
    for (const a of s.pendingApprovals) {
      if (a.state === "pending") pending.push({ sessionId: s.id, approval: a });
    }
  }

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["tenant-agent-sessions", tenant] });
    void queryClient.invalidateQueries({ queryKey: ["tenant-agent", tenant] });
  };

  const decide = async (id: string, approve: boolean) => {
    setRefusal(null);
    try {
      if (approve) await approveAssistantApproval(id);
      else await denyAssistantApproval(id, { reason: reasons[id] ?? "" });
      refresh();
    } catch (err) {
      if (err instanceof ApiError && err.code === "approval_expired") {
        setRefusal(labelFor("errorCodes", "approval_expired"));
        refresh();
        return;
      }
      setRefusal("The decision was refused. Reload and try again.");
    }
  };

  return (
    <Card title="Approvals">
      {list.isPending && <p>Loading approvals…</p>}
      {list.data && pending.length === 0 && <p>No approvals are waiting.</p>}
      {refusal && <p role="alert">{refusal}</p>}
      <ul className={styles.list}>
        {pending.map(({ approval: a }) => (
          <li key={a.id} className={styles.approval}>
            <div>
              <strong>{a.tool}</strong> on {a.targetKind} {a.targetRef}
            </div>
            <pre>{JSON.stringify(a.arguments, null, 2)}</pre>
            <div>Expires {new Date(a.expiresAt).toLocaleString()}</div>
            {canWrite && (
              <div className={styles.actions}>
                <button type="button" onClick={() => void decide(a.id, true)}>Approve</button>
                <label>
                  Reason
                  <input
                    value={reasons[a.id] ?? ""}
                    onChange={(e) => setReasons({ ...reasons, [a.id]: e.target.value })}
                  />
                </label>
                <button type="button" onClick={() => void decide(a.id, false)}>Deny</button>
              </div>
            )}
          </li>
        ))}
      </ul>
    </Card>
  );
}
