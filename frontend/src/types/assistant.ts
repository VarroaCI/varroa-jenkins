// Assistant and run note types (add-assistant-ui). The BFF writes these
// shapes; api/openapi/components/schemas.yaml is the contract.

/** The state of an assistant session, as the record store stores it. */
export type AssistantSessionState =
  | "running"
  | "waiting_approval"
  | "completed"
  | "failed"
  | "cancelled"
  | "skipped"
  | "expired";

/** Why a session was skipped instead of run. */
export type AssistantSkipReason = "budget_day" | "kill_switch" | "data_sharing_none" | "provider_unavailable";

/** The machine codes an assistant refusal carries. */
export type AssistantErrorCode =
  | "assistant_disabled"
  | "assistant_key_unavailable"
  | "assistant_off"
  | "assistant_busy"
  | "prompt_not_startable"
  | "kill_switch"
  | "approval_expired"
  | "team_agent_session";

/** The codes a note refusal carries. */
export type RunNoteErrorCode = "note_too_long" | "note_session_invalid";

/** The state of one approval. */
export type AssistantApprovalState = "pending" | "approved" | "denied" | "expired";

/** What an approval targets. */
export type AssistantTargetKind = "run" | "job" | "controller" | "none";

/** How much data a provider may receive. */
export type DataSharingLevel = "none" | "metadata" | "logs";

/** The event types the assistant stream carries. */
export type AssistantEventType =
  | "user"
  | "assistant"
  | "tool_call"
  | "tool_result"
  | "approval_requested"
  | "approval_resolved"
  | "error"
  | "done";

/** The context a session carries, as the client names it. */
export interface AssistantSessionContext {
  route?: string;
  team?: string;
  job?: string;
  run?: string;
  controller?: string;
  cell?: string;
  prompt?: string;
  message?: string;
  repo?: string;
}

/** One assistant session. */
export interface AssistantSession {
  id: string;
  kind: "assistant" | "team_agent";
  principal: string;
  team: string;
  cluster: string;
  trigger: string;
  context: AssistantSessionContext;
  provider: string;
  model: string;
  keySource: "chart" | "user";
  dataSharing: DataSharingLevel;
  state: AssistantSessionState;
  skipReason: AssistantSkipReason | null;
  promptTokens: number;
  completionTokens: number;
  steps: number;
  startedAt: string;
  endedAt: string | null;
}

export interface AssistantSessionList {
  items: AssistantSession[];
  nextCursor: string | null;
}

/** One approval request. */
export interface AssistantApproval {
  id: string;
  sessionId: string;
  callId: string;
  tool: string;
  arguments: unknown;
  targetKind: AssistantTargetKind;
  targetRef: string;
  state: AssistantApprovalState;
  requestedAt: string;
  expiresAt: string;
  resolvedBy: string | null;
  resolvedAt: string | null;
  reason: string | null;
  /** The tool result text. Null until the approval runs. */
  result: string | null;
}

export interface AssistantApprovalList {
  items: AssistantApproval[];
  nextCursor: string | null;
}

export interface AssistantSessionDetail {
  session: AssistantSession;
  approvals: AssistantApproval[];
}

export interface AssistantApprovalOutcome {
  approval: AssistantApproval;
  result: { content: string; isError: boolean };
}

export interface AssistantAcceptedState {
  state: AssistantSessionState;
}

export interface AssistantLimits {
  budgets: { session: number; userDay: number; teamDay: number; steps: number };
  externalDataSharing: DataSharingLevel;
  approvalExpiry: string;
  egress: { networkPolicy: boolean; cidrs: string[]; ports: number[] };
}

export interface AssistantSettings {
  paused: boolean;
  pausedBy: string | null;
  pausedAt: string | null;
  defaultProvider: string;
  defaultModel: string;
  tenantDataSharing: Record<string, DataSharingLevel>;
  updatedAt: string;
  updatedBy: string;
  limits: AssistantLimits;
}

export interface AssistantSettingsUpdate {
  paused?: boolean;
  defaultProvider?: string;
  defaultModel?: string;
  tenantDataSharing?: {
    set?: { tenant: string; level: DataSharingLevel };
    remove?: string;
  };
}

export interface AssistantProvider {
  name: string;
  kind: string;
  models: string[];
  trust: string;
  default: boolean;
  available: boolean;
  baseURL: string;
}

export interface AssistantProviderList {
  items: AssistantProvider[];
}

/** The caller's own assistant view. */
export interface MyAssistant {
  available: boolean;
  enabled?: boolean;
  provider?: string;
  model?: string;
  dataSharing?: DataSharingLevel;
  sessionKey?: { provider: string } | null;
  effectiveLevels?: Record<string, DataSharingLevel>;
  dayTokens?: number;
  dayBudget?: number;
}

export interface MyAssistantUpdate {
  enabled: boolean;
  provider: string;
  model: string;
  dataSharing: DataSharingLevel;
}

export interface MyAssistantKeyRequest {
  provider: string;
  apiKey: string;
}

/** A note on one build, or on one job. */
export interface RunNote {
  id: string;
  runId: string | null;
  cluster: string;
  team: string;
  jobPath: string;
  principal: string;
  sessionId: string | null;
  body: string;
  createdAt: string;
  buildNumber?: number | null;
}

export interface RunNoteList {
  items: RunNote[];
}

export interface CreateRunNoteRequest {
  body?: string;
  sessionId?: string;
}

/** One assistant stream frame, after the hook parses it. */
export interface AssistantFrame {
  seq: number;
  type: AssistantEventType;
  data: Record<string, unknown> & { created_at: string };
}
