import { describe, it, expect } from "vitest";
import { applyEvent, emptyModel, type TranscriptModel } from "./transcript";
import { currentActivity, unechoedPending, APPROVAL_LABEL, SENDING_LABEL, THINKING_LABEL } from "./activity";
import type { AssistantFrame } from "../../types/assistant";

const at = "2026-10-31T10:00:00Z";

/** fold applies frames in order, numbering them from seq 1. */
function fold(...frames: [AssistantFrame["type"], Record<string, unknown>][]): TranscriptModel {
  let m: TranscriptModel = { ...emptyModel(), live: true };
  frames.forEach(([type, data], i) => {
    m = applyEvent(m, { seq: i + 1, type, data: { created_at: at, ...data } });
  });
  return m;
}

const user = (text = "hi"): [AssistantFrame["type"], Record<string, unknown>] => ["user", { text }];
const call = (name: string, id = "c1"): [AssistantFrame["type"], Record<string, unknown>] =>
  ["tool_call", { call_id: id, name, arguments: {}, class: "read" }];

describe("currentActivity", () => {
  it("names a sent message while it is sending", () => {
    const a = currentActivity(emptyModel(), null, { text: "hi", afterSeq: 0, sending: true });
    expect(a).toEqual({ kind: "sending", label: SENDING_LABEL });
  });

  it("names thinking once a sent message is accepted", () => {
    const a = currentActivity(emptyModel(), null, { text: "hi", afterSeq: 0, sending: false });
    expect(a?.label).toBe(THINKING_LABEL);
  });

  it("names thinking after the user row arrives", () => {
    expect(currentActivity(fold(user()), null, null)?.label).toBe(THINKING_LABEL);
  });

  it("shows nothing while a turn has text", () => {
    const m = fold(user(), ["assistant", { turn: 1, text: "Hello", final: false }]);
    expect(currentActivity(m, null, null)).toBeNull();
  });

  it("thinks on an empty final turn", () => {
    const m = fold(user(), ["assistant", { turn: 1, text: "", final: true }]);
    expect(currentActivity(m, null, null)?.label).toBe(THINKING_LABEL);
  });

  it("names a tool call that has no result", () => {
    const m = fold(user(), call("get_run"));
    expect(currentActivity(m, null, null)?.label).toBe("get_run · In progress");
  });

  it("thinks after a tool result", () => {
    const m = fold(user(), call("get_run"), ["tool_result", { call_id: "c1", content: "ok", is_error: false }]);
    expect(currentActivity(m, null, null)?.label).toBe(THINKING_LABEL);
  });

  it("names a pending approval", () => {
    const m = fold(
      user(),
      call("restart_run"),
      ["approval_requested", { approval_id: "a1", call_id: "c1", tool: "restart_run", target_kind: "run", target_ref: "r1" }],
    );
    expect(currentActivity(m, "waiting_approval", null)?.label).toBe("restart_run · Awaiting approval");
  });

  it("names a parked session with no call", () => {
    const m = fold(user(), ["done", { state: "waiting_approval" }]);
    expect(currentActivity(m, null, null)?.label).toBe(APPROVAL_LABEL);
  });

  it("shows nothing after done", () => {
    const m = fold(user(), ["assistant", { turn: 1, text: "Done.", final: true }], ["done", { state: "completed" }]);
    expect(currentActivity(m, null, null)).toBeNull();
  });

  it("shows nothing after an error", () => {
    expect(currentActivity(fold(user(), ["error", { code: "x", message: "y" }]), null, null)).toBeNull();
  });

  it("trusts a completed session over an unfolded replay", () => {
    const replay = { ...fold(user()), live: false };
    expect(currentActivity(replay, "completed", null)).toBeNull();
  });

  it("falls back to the session state when no user row exists", () => {
    expect(currentActivity(emptyModel(), "running", null)?.label).toBe(THINKING_LABEL);
  });
});

describe("unechoedPending", () => {
  it("hides a pending message once a user row after it arrives", () => {
    const m = fold(user("first"), user("second"));
    expect(unechoedPending(m, { text: "x", afterSeq: 1, sending: true })).toBeNull();
  });

  it("keeps a pending message while no later user row has arrived", () => {
    const m = fold(user("first"));
    const p = { text: "x", afterSeq: 1, sending: true };
    expect(unechoedPending(m, p)).toBe(p);
  });
});
