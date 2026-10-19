import { describe, it, expect } from "vitest";
import { applyEvent, callDurationMs, callLabel, cutUtf8, emptyModel, type CallView } from "./transcript";
import type { AssistantFrame } from "../../types/assistant";

const at = "2026-10-18T10:00:00Z";
const frame = (seq: number, type: AssistantFrame["type"], data: Record<string, unknown> = {}): AssistantFrame => ({
  seq,
  type,
  data: { created_at: at, ...data },
});

describe("applyEvent", () => {
  it("folds a whole turn into rows and joins the chunks", () => {
    let m = emptyModel();
    m = applyEvent(m, frame(1, "user", { text: "hi" }));
    m = applyEvent(m, frame(2, "assistant", { turn: 1, text: "Hel" }));
    m = applyEvent(m, frame(3, "assistant", { turn: 1, text: "lo", final: true }));
    expect(m.rows).toEqual([
      { kind: "user", seq: 1, text: "hi" },
      { kind: "turn", turn: 1 },
    ]);
    expect(m.turns[1]).toEqual({ text: "Hello", final: true });
    expect(m.lastSeq).toBe(3);
  });

  it("ignores a replayed seq", () => {
    const m = applyEvent(emptyModel(), frame(4, "user", { text: "once" }));
    expect(applyEvent(m, frame(4, "user", { text: "again" }))).toBe(m);
    expect(applyEvent(m, frame(2, "user", { text: "older" }))).toBe(m);
  });

  it("pairs a tool result with its call, then an approval with the call", () => {
    let m = emptyModel();
    m = applyEvent(m, frame(1, "tool_call", { call_id: "c1", name: "restart_run", arguments: { run: "r1" }, class: "write" }));
    m = applyEvent(m, frame(2, "approval_requested", { approval_id: "a1", call_id: "c1", tool: "restart_run", target_kind: "run", target_ref: "r1", expires_at: "2099-01-01T00:00:00Z" }));
    expect(m.calls.c1.approval?.state).toBe("pending");
    expect(callLabel(m.calls.c1)).toBe("Awaiting approval");
    m = applyEvent(m, frame(3, "approval_resolved", { approval_id: "a1", state: "approved", resolved_by: "alice" }));
    expect(m.calls.c1.approval?.state).toBe("approved");
    expect(m.calls.c1.approval?.resolvedBy).toBe("alice");
    m = applyEvent(m, frame(4, "tool_result", { call_id: "c1", name: "restart_run", content: "ok", is_error: false, data_level: "logs" }));
    expect(callLabel(m.calls.c1)).toBe("Returned");
    expect(m.calls.c1.result?.dataLevel).toBe("logs");
  });

  it("drops a result for an unknown call and an approval for an unknown id", () => {
    let m = emptyModel();
    m = applyEvent(m, frame(1, "tool_result", { call_id: "ghost", content: "x" }));
    m = applyEvent(m, frame(2, "approval_resolved", { approval_id: "nope", state: "denied" }));
    expect(m.calls).toEqual({});
    expect(m.lastSeq).toBe(2);
  });

  it("keeps errors and records the done frame", () => {
    let m = emptyModel();
    m = applyEvent(m, frame(1, "error", { code: "provider_failed", message: "down" }));
    m = applyEvent(m, frame(2, "done", { state: "failed", stop: "error", usage_estimated: true }));
    expect(m.rows[0]).toEqual({ kind: "error", seq: 1, code: "provider_failed", message: "down" });
    expect(m.state).toBe("failed");
    expect(m.stop).toBe("error");
    expect(m.usageEstimated).toBe(true);
  });

  it("answers the matching label for each approval state", () => {
    const base: CallView = { callId: "c", name: "n", arguments: {}, class: "write", createdAt: at };
    const approval = (state: "pending" | "denied" | "expired" | "approved") => ({
      id: "a", sessionId: "s", tool: "n", arguments: {}, targetKind: "run", targetRef: "r", state, expiresAt: at,
    });
    expect(callLabel({ ...base, approval: approval("denied") })).toBe("Denied");
    expect(callLabel({ ...base, approval: approval("expired") })).toBe("Expired");
    expect(callLabel({ ...base, result: { content: "x", isError: true, dataLevel: "logs", createdAt: at } })).toBe("Error");
    expect(callLabel(base)).toBe("In progress");
  });
});

describe("follow-up turns", () => {
  it("clears the state of the last turn when a follow-up starts", () => {
    let m = applyEvent(emptyModel(), frame(1, "done", { state: "completed" }));
    expect(m.state).toBe("completed");
    m = applyEvent(m, frame(2, "user", { text: "and the fix?" }));
    expect(m.state).toBeNull();
  });
});

describe("callDurationMs", () => {
  it("measures call to result and is null without a result", () => {
    const base: CallView = { callId: "c", name: "n", arguments: {}, class: "read", createdAt: "2026-10-18T10:00:00Z" };
    expect(callDurationMs(base)).toBeNull();
    const done = { ...base, result: { content: "", isError: false, dataLevel: "logs", createdAt: "2026-10-18T10:00:02Z" } };
    expect(callDurationMs(done)).toBe(2000);
    const bad = { ...base, result: { ...done.result, createdAt: "nope" } };
    expect(callDurationMs(bad)).toBeNull();
  });
});

describe("cutUtf8", () => {
  it("keeps a short string and never splits a multi-byte character", () => {
    expect(cutUtf8("abc", 10)).toBe("abc");
    const cut = cutUtf8("é".repeat(10), 5);
    expect(new TextEncoder().encode(cut).length).toBeLessThanOrEqual(5);
    expect(cut).toBe("éé");
  });
});
