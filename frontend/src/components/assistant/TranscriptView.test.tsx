import { describe, it, expect, vi, afterEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { TranscriptView } from "./TranscriptView";
import { emptyModel, type TranscriptModel } from "./transcript";
import { REDUCED_MOTION_QUERY } from "../../hooks/useReducedMotion";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function callModel(result: TranscriptModel["calls"][string]["result"]): TranscriptModel {
  return {
    ...emptyModel(),
    rows: [{ kind: "call", callId: "c1" }],
    calls: {
      c1: {
        callId: "c1",
        name: "restart_run",
        arguments: { run: "r1" },
        class: "write",
        createdAt: "2026-10-22T10:00:00Z",
        result,
      },
    },
  };
}

describe("TranscriptView", () => {
  it("renders a notice and no result block for a withheld result", () => {
    const { container } = render(
      <TranscriptView
        model={callModel({
          content: "",
          isError: false,
          dataLevel: "logs",
          createdAt: "2026-10-22T10:00:01Z",
          withheld: true,
        })}
      />,
    );
    expect(screen.getByText("Tool result withheld. You do not have permission to view it.")).toBeTruthy();
    expect(container.querySelectorAll("pre")).toHaveLength(1);
    expect(container.querySelector("pre")?.textContent).toContain("\"run\": \"r1\"");
  });

  it("renders the content of an ordinary result", () => {
    const { container } = render(
      <TranscriptView
        model={callModel({
          content: "ok:restart_run",
          isError: false,
          dataLevel: "logs",
          createdAt: "2026-10-22T10:00:01Z",
          withheld: false,
        })}
      />,
    );
    expect(screen.queryByText(/withheld/)).toBeNull();
    expect(container.querySelectorAll("pre")).toHaveLength(2);
    expect(screen.getByText("ok:restart_run")).toBeTruthy();
  });

  it("names the open tool call in the status line", () => {
    const m = { ...callModel(undefined), state: "running" as const };
    render(<TranscriptView model={m} />);
    expect(screen.getByRole("status").textContent).toContain("restart_run · In progress");
  });

  it("shows nothing in the status line while text streams", () => {
    const m: TranscriptModel = {
      ...emptyModel(),
      rows: [{ kind: "turn", turn: 1 }],
      turns: { 1: { text: "Hello", final: false } },
    };
    render(<TranscriptView model={m} />);
    expect(screen.getByRole("status").textContent).toBe("");
  });

  it("renders a pending message after the rows", () => {
    render(<TranscriptView model={emptyModel()} pending={{ text: "queued", afterSeq: 0, sending: true }} />);
    expect(screen.getByText("queued")).toBeTruthy();
    expect(screen.getByRole("status").textContent).toContain("Sending…");
  });

  it("opens a call row while its approval is pending", () => {
    const m = callModel(undefined);
    m.calls.c1.approval = {
      id: "a1", sessionId: "s1", callId: "c1", tool: "restart_run", arguments: {},
      targetKind: "run", targetRef: "r1", state: "pending", expiresAt: "2026-10-22T10:05:00Z",
    };
    const { container } = render(<TranscriptView model={m} approvalActions={() => <button type="button">Approve</button>} />);
    expect(container.querySelector("details")?.hasAttribute("open")).toBe(true);
    expect(screen.getByRole("button", { name: "Approve" })).toBeTruthy();
  });

  it("skips an empty final turn", () => {
    const m: TranscriptModel = {
      ...emptyModel(),
      rows: [{ kind: "turn", turn: 1 }],
      turns: { 1: { text: "", final: true } },
    };
    render(<TranscriptView model={m} />);
    expect(screen.queryByText("…")).toBeNull();
    expect(screen.getByRole("status").textContent).toBe("");
  });

  it("renders replayed text at once", () => {
    const m: TranscriptModel = {
      ...emptyModel(),
      live: false,
      rows: [{ kind: "turn", turn: 1 }],
      turns: { 1: { text: "Replayed text", final: false } },
    };
    render(<TranscriptView model={m} />);
    expect(screen.getByText("Replayed text")).toBeTruthy();
  });

  it("reveals live text over one window", () => {
    vi.useFakeTimers({ toFake: ["requestAnimationFrame", "cancelAnimationFrame"] });
    const m: TranscriptModel = {
      ...emptyModel(),
      live: true,
      rows: [{ kind: "turn", turn: 1 }],
      turns: { 1: { text: "alpha beta gamma", final: true } },
    };
    render(<TranscriptView model={m} />);
    expect(screen.queryByText("alpha beta gamma")).toBeNull();
    act(() => vi.advanceTimersByTime(600));
    expect(screen.getByText("alpha beta gamma")).toBeTruthy();
  });

  it("renders live text at once under reduced motion", () => {
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: query === REDUCED_MOTION_QUERY,
      media: query,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
    }));
    const m: TranscriptModel = {
      ...emptyModel(),
      live: true,
      rows: [{ kind: "turn", turn: 1 }],
      turns: { 1: { text: "calm text", final: true } },
    };
    render(<TranscriptView model={m} />);
    expect(screen.getByText("calm text")).toBeTruthy();
  });
});
