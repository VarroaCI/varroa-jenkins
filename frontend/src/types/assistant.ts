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
  | "tenant_agent_session"
  | "record_store_unreachable";

/** The codes a note refusal carries. */
export type RunNoteErrorCode = "note_too_long" | "note_session_invalid";

/** The codes an error event on the stream carries. */
export type StreamErrorCode =
  | "budget_session"
  | "budget_day"
  | "steps"
  | "kill_switch"
  | "provider_error"
  | "session_key_missing"
  | "lease_lost"
  | "data_level_lowered"
  | "data_level_unchecked";

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
  tenant?: string;
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
  kind: "assistant" | "tenant_agent";
  principal: string;
  tenant: string;
  cluster: string;
  trigger: string;
  context: AssistantSessionContext;
  /** The first user message as one line of plain text, at most 120 characters. Empty when the session has no user message. */
  firstMessage: string;
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
  /** The tool result text. Null until the approval runs, and null when withheld. */
  result: string | null;
  /** True when the caller may view the session but not its tool result. */
  resultWithheld: boolean;
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

/** The kill switch and the tenant levels. An admin also sees the tenant levels. */
export interface AssistantSettings {
  paused: boolean;
  pausedBy?: string | null;
  pausedAt?: string | null;
  tenantDataSharing?: Record<string, DataSharingLevel>;
  updatedAt?: string;
  updatedBy?: string;
}

/** The kill switch change that PUT /assistant/settings takes. */
export interface AssistantSettingsUpdate {
  paused?: boolean;
  tenantDataSharing?: {
    set?: { tenant: string; level: DataSharingLevel };
    remove?: string;
  };
}

/** One model provider of the AssistantConfig object. */
export interface AssistantConfigProvider {
  name: string;
  kind: "anthropic" | "openai-compatible";
  baseURL?: string;
  models: string[];
  trust: "internal" | "external";
  auth?: "apiKey" | "none";
  cidrs?: string[];
  port?: number;
}

/** The spec of the AssistantConfig object varroa-assistant. */
export interface AssistantConfigSpec {
  providers: AssistantConfigProvider[];
  defaultProvider?: string;
  defaultModel?: string;
  externalDataSharing: DataSharingLevel;
  budgets: { session: number; userDay: number; tenantDay: number; steps: number };
  approvalExpiry: string;
}

/** The AssistantConfig object that GET and PUT /assistant/config carry. */
export interface AssistantConfigObject {
  apiVersion?: string;
  kind?: string;
  metadata: { name: string; resourceVersion?: string };
  spec: AssistantConfigSpec;
  status?: {
    observedGeneration?: number;
    providers?: { name: string; keySet: boolean }[];
    conditions?: { type: string; status: string; reason?: string; message?: string }[];
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
  tenant: string;
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
