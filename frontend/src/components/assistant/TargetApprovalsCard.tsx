// TargetApprovalsCard lists the approvals of one object: a build, a job, or a
// controller. Pending approvals come first, then the ten newest resolved.
import { useQuery } from "@tanstack/react-query";
import { listAssistantApprovals } from "../../api/client";
import { Card } from "../Card";
import { useAssistant } from "./AssistantProvider";
import { ApprovalRow } from "./ApprovalRow";
import type { ApprovalRun } from "./approvalHint";
import type { ApprovalView } from "./transcript";
import type { AssistantApproval } from "../../types/assistant";

export interface TargetApprovalsCardProps {
  kind: "run" | "job" | "controller";
  targetRef: string;
  run?: ApprovalRun;
  viewer: string;
}

const RESOLVED_LIMIT = 10;

function toView(a: AssistantApproval): ApprovalView {
  return {
    id: a.id,
    sessionId: a.sessionId,
    callId: a.callId,
    tool: a.tool,
    arguments: a.arguments,
    targetKind: a.targetKind,
    targetRef: a.targetRef,
    state: a.state,
    expiresAt: a.expiresAt,
    resolvedBy: a.resolvedBy ?? undefined,
    resolvedAt: a.resolvedAt ?? undefined,
  };
}

/** orderApprovals puts pending first, then the newest resolved ones. */
export function orderApprovals(items: AssistantApproval[]): AssistantApproval[] {
  const pending = items.filter((a) => a.state === "pending");
  const resolved = items
    .filter((a) => a.state !== "pending")
    .sort((x, y) => (y.resolvedAt ?? "").localeCompare(x.resolvedAt ?? ""))
    .slice(0, RESOLVED_LIMIT);
  return [...pending, ...resolved];
}

/** TargetApprovalsCard renders the approvals of one target. It is empty when there are none. */
export function TargetApprovalsCard({ kind, targetRef, run, viewer }: TargetApprovalsCardProps) {
  const assistant = useAssistant();
  const list = useQuery({
    queryKey: ["assistant-approvals", kind, targetRef],
    queryFn: () => listAssistantApprovals({ targetKind: kind, targetRef }),
    enabled: assistant.canUse,
    retry: false,
  });
  const items = orderApprovals(list.data?.items ?? []);
  if (!assistant.canUse || items.length === 0) return null;
  return (
    <Card title="Assistant approvals">
      {items.map((a) => (
        <ApprovalRow
          key={a.id}
          approval={toView(a)}
          viewer={viewer}
          principal={viewer}
          run={run}
        />
      ))}
    </Card>
  );
}
