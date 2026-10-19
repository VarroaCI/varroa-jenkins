import { describe, expect, it } from "vitest";
import type { AssistantApproval, AssistantSessionDetail, AssistantSessionState } from "../../types/assistant";
import { parsePrArgs, parsePrResult, proposeView } from "./proposeState";

const URL_TEXT = JSON.stringify({ url: "https://git.example.org/acme/payments/pulls/1", number: 1 });

function approval(overrides: Partial<AssistantApproval> = {}): AssistantApproval {
  return {
    id: "a1",
    sessionId: "s1",
    callId: "c1",
    tool: "open_pull_request",
    arguments: { repo: "https://git.example.org/acme/payments", branch: "varroa/propose-pipeline", title: "Add", body: "why", files: [{ path: "Jenkinsfile", content: "pipeline {}" }] },
    targetKind: "none",
    targetRef: "",
    state: "pending",
    requestedAt: "2026-10-19T10:00:00Z",
    expiresAt: "2026-10-19T11:00:00Z",
    resolvedBy: null,
    resolvedAt: null,
    reason: null,
    result: null,
    ...overrides,
  };
}

function detail(state: AssistantSessionState, approvals: AssistantApproval[] = [], extra: Record<string, unknown> = {}): AssistantSessionDetail {
  return {
    session: { id: "s1", state, steps: 3, skipReason: null, ...extra } as unknown as AssistantSessionDetail["session"],
    approvals,
  };
}

describe("proposeView", () => {
  it("shows the form before any session", () => {
    expect(proposeView(undefined)).toEqual({ kind: "form" });
  });

  it("reads while the session runs", () => {
    expect(proposeView(detail("running"))).toEqual({ kind: "reading", steps: 3 });
  });

  it("is ready when the newest proposal is pending", () => {
    const a = approval();
    const v = proposeView(detail("waiting_approval", [a]));
    expect(v.kind).toBe("ready");
    if (v.kind === "ready") expect(v.args.files).toEqual([{ path: "Jenkinsfile", content: "pipeline {}" }]);
  });

  it("waits for another tool than the proposal", () => {
    const other = approval({ id: "a2", tool: "restart_run" });
    expect(proposeView(detail("waiting_approval", [other]))).toEqual({ kind: "waiting", tool: "restart_run" });
  });

  it("shows the opened pull request while the session still runs", () => {
    const a = approval({ state: "approved", result: URL_TEXT });
    expect(proposeView(detail("running", [a]))).toEqual({
      kind: "opened", url: "https://git.example.org/acme/payments/pulls/1", number: 1,
    });
  });

  it("names the skip reason of a skipped session", () => {
    const v = proposeView(detail("skipped", [], { skipReason: "kill_switch" }));
    expect(v.kind).toBe("skipped");
    if (v.kind === "skipped") expect(v.reason.length).toBeGreaterThan(0);
  });

  it("reports an expired session", () => {
    expect(proposeView(detail("expired"))).toEqual({ kind: "expired" });
  });

  it("reports a failed pull request from the approval text", () => {
    const a = approval({ state: "approved", result: "forge_unreachable: down" });
    expect(proposeView(detail("completed", [a]))).toEqual({ kind: "failed", text: "forge_unreachable: down" });
  });

  it("reports a denied proposal", () => {
    expect(proposeView(detail("completed", [approval({ state: "denied" })]))).toEqual({ kind: "denied" });
  });

  it("reports a failed session with no proposal", () => {
    expect(proposeView(detail("failed"))).toMatchObject({ kind: "failed" });
  });

  it("reports a completed session that opened no pull request", () => {
    expect(proposeView(detail("completed"))).toEqual({ kind: "noProposal" });
  });

  it("lets the newest of two proposals decide", () => {
    const older = approval({ id: "old", state: "denied", requestedAt: "2026-10-19T09:00:00Z" });
    const newer = approval({ id: "new", state: "pending", requestedAt: "2026-10-19T10:00:00Z" });
    const v = proposeView(detail("waiting_approval", [newer, older]));
    expect(v.kind).toBe("ready");
    if (v.kind === "ready") expect(v.approval.id).toBe("new");
  });
});

describe("parsePrResult", () => {
  it("reads a URL and a number", () => {
    expect(parsePrResult(URL_TEXT)).toEqual({ url: "https://git.example.org/acme/payments/pulls/1", number: 1 });
  });

  it("answers null for an error text and for no text", () => {
    expect(parsePrResult("forge_unreachable: down")).toBeNull();
    expect(parsePrResult(null)).toBeNull();
  });
});

describe("parsePrArgs", () => {
  it("reads missing fields as empty", () => {
    expect(parsePrArgs(undefined)).toEqual({ repo: "", branch: "", title: "", body: "", files: [] });
  });

  it("drops file entries that are not text", () => {
    const args = parsePrArgs({ files: [{ path: "a", content: "x" }, { path: 1 }, "b"] });
    expect(args.files).toEqual([{ path: "a", content: "x" }]);
  });
});
