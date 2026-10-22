// The transcript reducer of the assistant drawer. It folds stream frames into
// rows. A frame whose seq is at or below the last seq is a replay, and it
// changes nothing.
import type { AssistantFrame, AssistantSessionState } from "../../types/assistant";

/** ApprovalView is the approval the drawer shows beside a tool call. */
export interface ApprovalView {
  id: string;
  sessionId: string;
  callId?: string;
  tool: string;
  arguments: unknown;
  targetKind: string;
  targetRef: string;
  state: "pending" | "approved" | "denied" | "expired";
  expiresAt: string;
  resolvedBy?: string;
  resolvedAt?: string;
}

/** CallView is one tool call with its result and approval, when it has them. */
export interface CallView {
  callId: string;
  name: string;
  arguments: unknown;
  class: string;
  createdAt: string;
  /** withheld is true when the caller may not read the result content. */
  result?: { content: string; isError: boolean; dataLevel: string; createdAt: string; withheld: boolean };
  approval?: ApprovalView;
}

/** A row of the transcript, in event order. */
export type TranscriptRow =
  | { kind: "user"; seq: number; text: string }
  | { kind: "turn"; turn: number }
  | { kind: "call"; callId: string }
  | { kind: "error"; seq: number; code: string; message: string };

/** TranscriptModel is the folded state of one session's stream. */
export interface TranscriptModel {
  rows: TranscriptRow[];
  turns: Record<number, { text: string; final: boolean }>;
  calls: Record<string, CallView>;
  approvals: Record<string, string>;
  lastSeq: number;
  state: AssistantSessionState | null;
  stop: string;
  usageEstimated: boolean;
}

/** emptyModel returns the state before any frame arrives. */
export function emptyModel(): TranscriptModel {
  return { rows: [], turns: {}, calls: {}, approvals: {}, lastSeq: 0, state: null, stop: "", usageEstimated: false };
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

/** applyEvent folds one frame into a new model. It never mutates model. */
export function applyEvent(model: TranscriptModel, frame: AssistantFrame): TranscriptModel {
  if (frame.seq <= model.lastSeq) return model;
  const next: TranscriptModel = {
    ...model,
    rows: [...model.rows],
    turns: { ...model.turns },
    calls: { ...model.calls },
    approvals: { ...model.approvals },
    lastSeq: frame.seq,
  };
  const d = frame.data;
  const createdAt = str(d.created_at);
  switch (frame.type) {
    case "user":
      // A follow-up starts a new turn, so the state of the last one is stale.
      next.state = null;
      next.rows.push({ kind: "user", seq: frame.seq, text: str(d.text) });
      break;
    case "assistant": {
      const turn = Number(d.turn ?? 0);
      const prev = next.turns[turn] ?? { text: "", final: false };
      next.turns[turn] = { text: prev.text + str(d.text), final: prev.final || d.final === true };
      if (!next.rows.some((r) => r.kind === "turn" && r.turn === turn)) {
        next.rows.push({ kind: "turn", turn });
      }
      break;
    }
    case "tool_call": {
      const callId = str(d.call_id);
      next.calls[callId] = {
        callId,
        name: str(d.name),
        arguments: d.arguments,
        class: str(d.class),
        createdAt,
      };
      next.rows.push({ kind: "call", callId });
      break;
    }
    case "tool_result": {
      const callId = str(d.call_id);
      const call = next.calls[callId];
      if (call) {
        next.calls[callId] = {
          ...call,
          result: {
            content: str(d.content),
            isError: d.is_error === true,
            dataLevel: str(d.data_level),
            createdAt,
            withheld: d.withheld === true,
          },
        };
      }
      break;
    }
    case "approval_requested": {
      const id = str(d.approval_id);
      const callId = str(d.call_id);
      const approval: ApprovalView = {
        id,
        sessionId: "",
        callId,
        tool: str(d.tool),
        arguments: d.arguments,
        targetKind: str(d.target_kind),
        targetRef: str(d.target_ref),
        state: "pending",
        expiresAt: str(d.expires_at),
      };
      next.approvals[id] = callId;
      const call = next.calls[callId];
      if (call) next.calls[callId] = { ...call, approval };
      break;
    }
    case "approval_resolved": {
      const callId = next.approvals[str(d.approval_id)];
      const call = callId ? next.calls[callId] : undefined;
      if (call?.approval) {
        const state = str(d.state) as ApprovalView["state"];
        next.calls[call.callId] = {
          ...call,
          approval: {
            ...call.approval,
            state,
            resolvedBy: str(d.resolved_by) || undefined,
            resolvedAt: createdAt,
          },
        };
      }
      break;
    }
    case "error":
      next.rows.push({ kind: "error", seq: frame.seq, code: str(d.code), message: str(d.message) });
      break;
    case "done":
      next.state = (str(d.state) || null) as AssistantSessionState | null;
      next.stop = str(d.stop);
      next.usageEstimated = d.usage_estimated === true;
      break;
  }
  return next;
}

/** callLabel names the state of a call for the row summary. */
export function callLabel(call: CallView): string {
  const a = call.approval;
  if (a?.state === "pending") return "Awaiting approval";
  if (a?.state === "denied") return "Denied";
  if (a?.state === "expired") return "Expired";
  if (call.result) return call.result.isError ? "Error" : "Returned";
  return "In progress";
}

/** callDurationMs is the time from the call to its result, or null. */
export function callDurationMs(call: CallView): number | null {
  if (!call.result) return null;
  const start = Date.parse(call.createdAt);
  const end = Date.parse(call.result.createdAt);
  if (Number.isNaN(start) || Number.isNaN(end)) return null;
  return Math.max(0, end - start);
}

/** cutUtf8 cuts s to at most maxBytes, never inside a multi-byte character. */
export function cutUtf8(s: string, maxBytes: number): string {
  const enc = new TextEncoder();
  const bytes = enc.encode(s);
  if (bytes.length <= maxBytes) return s;
  let end = maxBytes;
  // A continuation byte has the bit pattern 10xxxxxx. Step back past them.
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) end--;
  return new TextDecoder().decode(bytes.slice(0, end));
}
