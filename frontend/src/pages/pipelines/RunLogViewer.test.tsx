import { createRef } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import { RunLogViewer, type RunLogViewerHandle } from "./RunLogViewer";

// JSDOM has no real layout, so the virtualizer is mocked to render every row
// and to expose a spy for scrollToIndex — the same convention
// ActivityTimeline.test.tsx uses for this package.
const mockScrollToIndex = vi.fn();
vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: (opts: { count: number; estimateSize: (i: number) => number }) => {
    const items: { key: number; index: number; start: number; size: number }[] = [];
    for (let i = 0; i < opts.count; i++) {
      items.push({ key: i, index: i, start: i * opts.estimateSize(i), size: opts.estimateSize(i) });
    }
    return {
      getVirtualItems: () => items,
      getTotalSize: () => opts.count * opts.estimateSize(0),
      scrollToIndex: mockScrollToIndex,
      measureElement: () => {},
    };
  },
}));

beforeEach(() => {
  mockScrollToIndex.mockClear();
  window.location.hash = "";
});

function lineEvent(n: number, text: string) {
  return { type: "line", data: { n, off: n * 10, ts: 1700000000000, text } };
}

describe("RunLogViewer", () => {
  it("renders lines from the log stream", () => {
    const { rerender } = render(<RunLogViewer runId="r1" runTerminal={false} lastEvent={null} />);
    rerender(<RunLogViewer runId="r1" runTerminal={false} lastEvent={lineEvent(1, "hello world")} />);
    expect(screen.getByText("hello world")).toBeInTheDocument();
  });

  it("turns follow off when the user scrolls up", () => {
    render(<RunLogViewer runId="r1" runTerminal={false} lastEvent={null} />);
    const followBtn = screen.getByRole("button", { name: "Follow" });
    expect(followBtn).toHaveAttribute("aria-pressed", "true");

    const scroller = screen.getByLabelText("Console log");
    // Scroll down first, then up — only a decrease from the previous
    // position counts as "the user scrolled up".
    fireEvent.scroll(scroller, { target: { scrollTop: 200 } });
    expect(followBtn).toHaveAttribute("aria-pressed", "true");
    fireEvent.scroll(scroller, { target: { scrollTop: 100 } });
    expect(followBtn).toHaveAttribute("aria-pressed", "false");
  });

  it("scrolls to and highlights a line permalink", () => {
    window.location.hash = "#L2";
    const { rerender } = render(<RunLogViewer runId="r1" runTerminal={false} lastEvent={null} />);
    rerender(<RunLogViewer runId="r1" runTerminal={false} lastEvent={lineEvent(1, "first")} />);
    rerender(<RunLogViewer runId="r1" runTerminal={false} lastEvent={lineEvent(2, "second")} />);

    expect(mockScrollToIndex).toHaveBeenCalledWith(1, { align: "center" });
    const el = document.getElementById("L2");
    expect(el?.className).toMatch(/highlighted/);
  });

  it("shows the first-error control from the meta event and scrolls to it", () => {
    const { rerender } = render(<RunLogViewer runId="r1" runTerminal={false} lastEvent={null} />);
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false}
        lastEvent={{
          type: "meta",
          data: { runId: "r1", chunkCount: 1, byteCount: 10, droppedBytes: 0, complete: true, status: "SUCCESS", firstErrorLine: 3 },
        }}
      />,
    );
    rerender(<RunLogViewer runId="r1" runTerminal={false} lastEvent={lineEvent(3, "boom: error occurred")} />);

    const btn = screen.getByRole("button", { name: "First error · line 3" });
    fireEvent.click(btn);
    expect(mockScrollToIndex).toHaveBeenCalledWith(0, { align: "center" });
  });

  it("shows a sticky-style header from a stage event and a gap row from a gap event", () => {
    const { rerender } = render(<RunLogViewer runId="r1" runTerminal={false} lastEvent={null} />);
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false}
        lastEvent={{ type: "stage", data: { stageId: "1", name: "Build", status: "IN_PROGRESS", startOffset: 0, startLine: 1 } }}
      />,
    );
    expect(screen.getAllByText("Build — IN_PROGRESS").length).toBeGreaterThan(0);

    rerender(
      <RunLogViewer runId="r1" runTerminal={false} lastEvent={{ type: "gap", data: { seq: 4, off: 100 } }} />,
    );
    expect(screen.getByText("Missing bytes at sequence 4.")).toBeInTheDocument();
  });

  it("states the log is still being written for a stopped run with no end event", () => {
    render(<RunLogViewer runId="r1" runTerminal lastEvent={null} />);
    expect(screen.getByText("The log is still being written.")).toBeInTheDocument();
  });

  it("shows an incomplete-log banner naming the reason from the end event", () => {
    const { rerender } = render(<RunLogViewer runId="r1" runTerminal lastEvent={null} />);
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal
        lastEvent={{ type: "end", data: { result: "FAILURE", logComplete: false, incompleteReason: "chunks lost" } }}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("chunks lost");
    expect(screen.queryByText("The log is still being written.")).not.toBeInTheDocument();
  });

  it("exposes scrollToStage on its imperative handle, for the graph's node selection", () => {
    const handleRef = createRef<RunLogViewerHandle>();
    const { rerender } = render(<RunLogViewer ref={handleRef} runId="r1" runTerminal={false} lastEvent={null} />);
    rerender(
      <RunLogViewer
        ref={handleRef}
        runId="r1"
        runTerminal={false}
        lastEvent={{ type: "stage", data: { stageId: "19", name: "Unit tests", status: "SUCCESS", startOffset: 0, startLine: 1 } }}
      />,
    );
    handleRef.current?.scrollToStage("19");
    expect(mockScrollToIndex).toHaveBeenCalledWith(0, { align: "start" });
  });

  it("exposes focusSearch, toggleFollow, and toggleTimestamps on its imperative handle", () => {
    const handleRef = createRef<RunLogViewerHandle>();
    render(<RunLogViewer ref={handleRef} runId="r1" runTerminal={false} lastEvent={null} />);
    act(() => handleRef.current?.toggleTimestamps());
    act(() => handleRef.current?.toggleFollow());
    expect(screen.getByRole("button", { name: "Follow" })).toHaveAttribute("aria-pressed", "false");
    act(() => handleRef.current?.focusSearch());
    expect(screen.getByLabelText("Search the log")).toHaveFocus();
  });
});
