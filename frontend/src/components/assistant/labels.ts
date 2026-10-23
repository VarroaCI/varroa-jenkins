// Labels for every state, skip reason, error code, and HTTP code the
// assistant returns. missingLabels reports any value with no label, so a
// test fails when the registry grows past the copy.
import type {
  AssistantApprovalState,
  AssistantErrorCode,
  AssistantSessionState,
  AssistantSkipReason,
  RunNoteErrorCode,
} from "../../types/assistant";

/** A1_VALUES lists every value of the A1 registry the drawer must label. */
export const A1_VALUES = {
  sessionStates: ["running", "waiting_approval", "completed", "failed", "cancelled", "skipped", "expired"] as AssistantSessionState[],
  skipReasons: ["budget_day", "kill_switch", "data_sharing_none", "provider_unavailable"] as AssistantSkipReason[],
  errorCodes: [
    "assistant_disabled", "assistant_key_unavailable", "assistant_off", "assistant_busy", "prompt_not_startable", "kill_switch",
    "approval_expired", "tenant_agent_session",
  ] as AssistantErrorCode[],
  httpCodes: [
    "assistant_disabled", "assistant_key_unavailable", "assistant_off", "assistant_busy", "prompt_not_startable", "kill_switch",
    "approval_expired", "tenant_agent_session", "note_too_long", "note_session_invalid",
  ] as (AssistantErrorCode | RunNoteErrorCode)[],
  approvalStates: ["pending", "approved", "denied", "expired"] as AssistantApprovalState[],
};

export const STATE_LABELS: Record<AssistantSessionState, string> = {
  running: "Running",
  waiting_approval: "Waiting for approval",
  completed: "Completed",
  failed: "Failed",
  cancelled: "Cancelled",
  skipped: "Skipped",
  expired: "Expired",
};

export const SKIP_LABELS: Record<AssistantSkipReason, string> = {
  budget_day: "The daily token budget is spent.",
  kill_switch: "The assistant is paused.",
  data_sharing_none: "Data sharing is off for this tenant.",
  provider_unavailable: "The model provider is unavailable.",
};

export const ERROR_LABELS: Record<AssistantErrorCode, string> = {
  assistant_disabled: "The assistant is not enabled on this install.",
  assistant_key_unavailable: "Your model key is not available in this login.",
  assistant_off: "You have the assistant switched off.",
  assistant_busy: "You have too many conversations running. Wait for one to finish.",
  prompt_not_startable: "This prompt cannot start here.",
  kill_switch: "The assistant is paused by an administrator.",
  approval_expired: "This approval expired. Ask the assistant again.",
  tenant_agent_session: "A Tenant agent session takes no messages.",
};

export const HTTP_LABELS: Record<AssistantErrorCode | RunNoteErrorCode, string> = {
  ...ERROR_LABELS,
  note_too_long: "The note is longer than 16384 bytes.",
  note_session_invalid: "This conversation cannot write a note on this build.",
};

export const APPROVAL_LABELS: Record<AssistantApprovalState, string> = {
  pending: "Awaiting approval",
  approved: "Approved",
  denied: "Denied",
  expired: "Expired",
};

const TABLES = {
  sessionStates: STATE_LABELS,
  skipReasons: SKIP_LABELS,
  errorCodes: ERROR_LABELS,
  httpCodes: HTTP_LABELS,
  approvalStates: APPROVAL_LABELS,
} as const;

export type LabelKind = keyof typeof TABLES;

/** labelFor returns the copy for a value, or the value itself when none is set. */
export function labelFor(kind: LabelKind, value: string): string {
  const table = TABLES[kind] as Record<string, string>;
  return table[value] ?? value;
}

/** missingLabels lists "kind:value" for each registry value with no label. */
export function missingLabels(): string[] {
  const missing: string[] = [];
  for (const kind of Object.keys(A1_VALUES) as (keyof typeof A1_VALUES)[]) {
    const table = TABLES[kind] as Record<string, string>;
    for (const value of A1_VALUES[kind] as string[]) {
      if (!table[value]) missing.push(`${kind}:${value}`);
    }
  }
  return missing;
}
