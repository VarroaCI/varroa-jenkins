import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { TranscriptView } from "./TranscriptView";
import { emptyModel, type TranscriptModel } from "./transcript";

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
});
