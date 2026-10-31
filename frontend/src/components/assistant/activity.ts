// activity decides the status line under the transcript. It answers what the
// assistant does now: it sends, it thinks, it runs a tool, or it waits for an
// approval. The status line is empty while text streams and after done.
import type { AssistantSessionState } from "../../types/assistant";
import { callLabel, type TranscriptModel } from "./transcript";

/** PendingMessage is a sent message that the stream has not echoed yet. */
export interface PendingMessage { text: string; afterSeq: number; sending: boolean }
/** Activity is what the assistant does now. */
export interface Activity { kind: "sending" | "thinking" | "tool" | "approval"; label: string }

export const SENDING_LABEL = "Sending…";
export const THINKING_LABEL = "Thinking…";
export const APPROVAL_LABEL = "Waiting for approval";
const THINKING: Activity = { kind: "thinking", label: THINKING_LABEL };

/** unechoedPending answers pending until a user row after its afterSeq arrives. */
export function unechoedPending(model: TranscriptModel, pending: PendingMessage | null): PendingMessage | null {
  if (!pending) return null;
  return model.rows.some((r) => r.kind === "user" && r.seq > pending.afterSeq) ? null : pending;
}

/** currentActivity answers the status line, or null while text streams or the session is idle. */
export function currentActivity(model: TranscriptModel, sessionState: AssistantSessionState | null,
  pending: PendingMessage | null): Activity | null {
  if (pending) return pending.sending ? { kind: "sending", label: SENDING_LABEL } : THINKING;
  const rows = model.rows;
  let start = 0;
  for (let i = rows.length - 1; i >= 0; i--) if (rows[i].kind === "user") { start = i + 1; break; }
  // Every end writes a done event, so a user row with no done after it is a running exchange.
  // Before the replay is folded, the session state is the only source the view trusts.
  const state = model.state ?? (model.live && start > 0 ? "running" : sessionState);
  if (state !== "running" && state !== "waiting_approval") return null;
  for (let i = rows.length - 1; i >= start; i--) {
    const r = rows[i];
    if (r.kind !== "call") continue;
    const c = model.calls[r.callId];
    if (c?.approval?.state === "pending") return { kind: "approval", label: `${c.name} · ${callLabel(c)}` };
  }
  if (state === "waiting_approval") return { kind: "approval", label: APPROVAL_LABEL };
  const last = rows[rows.length - 1];
  if (last?.kind === "turn") return model.turns[last.turn]?.text ? null : THINKING;
  if (last?.kind === "error") return null;
  for (let i = rows.length - 1; i >= start; i--) {
    const r = rows[i];
    if (r.kind === "turn") break;
    if (r.kind !== "call") continue;
    const c = model.calls[r.callId];
    const ended = c?.approval?.state === "denied" || c?.approval?.state === "expired";
    if (c && !c.result && !ended) return { kind: "tool", label: `${c.name} · ${callLabel(c)}` };
  }
  return THINKING;
}
