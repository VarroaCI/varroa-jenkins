import { createRef } from "react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, fireEvent, render, screen } from "@testing-library/react";
import {
  RunLogViewer,
  type RunLogViewerEvent,
  type RunLogViewerHandle,
} from "./RunLogViewer";

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

const metaFrame: RunLogViewerEvent = {
  type: "meta",
  data: { runId: "r1", chunkCount: 1, byteCount: 10, droppedBytes: 0, complete: true, status: "SUCCESS" },
};

function lineEvent(n: number, text: string): RunLogViewerEvent {
  return { type: "line", data: { n, off: n * 10, ts: 1700000000000, text } };
}

describe("RunLogViewer", () => {
  it("renders every line of one batch of frames, and applies the end frame of that batch", () => {
    render(
      <RunLogViewer
        runId="r1"
        runTerminal logFinalized={false}
        frames={[
          lineEvent(1, "one"),
          lineEvent(2, "two"),
          lineEvent(3, "three"),
          { type: "end", data: { result: "SUCCESS", logComplete: true } },
        ]}
        version={1}
      />,
    );

    expect(screen.getByText("one")).toBeInTheDocument();
    expect(screen.getByText("two")).toBeInTheDocument();
    expect(screen.getByText("three")).toBeInTheDocument();
    expect(screen.queryByText("The log is still being written.")).not.toBeInTheDocument();
  });

  it("keeps many line frames in order in one commit, with a stage header in its place", () => {
    render(
      <RunLogViewer
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[
          lineEvent(1, "one"),
          lineEvent(2, "two"),
          { type: "stage", data: { stageId: "9", name: "Deploy", status: "IN_PROGRESS", startOffset: 200, startLine: 3 } },
          lineEvent(3, "three"),
          lineEvent(4, "four"),
        ]}
        version={1}
      />,
    );

    for (const text of ["one", "two", "three", "four"]) {
      expect(screen.getByText(text)).toBeInTheDocument();
    }

    // The mocked virtualizer renders every row, so the row elements give the
    // order the viewer committed: the two lines before the stage, then the
    // header, then the two lines after it.
    const order = Array.from(document.querySelectorAll("[data-index]")).map(
      (row) => row.firstElementChild?.id,
    );
    expect(order).toEqual(["L1", "L2", "stage-9", "L3", "L4"]);
  });

  it("applies the frames the page already holds when it mounts", () => {
    const frames = [lineEvent(1, "early"), lineEvent(2, "later")];
    const { unmount } = render(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={frames} version={2} />,
    );
    expect(screen.getByText("later")).toBeInTheDocument();
    unmount();

    // The page unmounts the viewer while another tab is active, so a mount
    // replays the frames the page kept.
    render(<RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={frames} version={2} />);
    expect(screen.getByText("early")).toBeInTheDocument();
    expect(screen.getByText("later")).toBeInTheDocument();
  });

  it("renders lines from the log stream", () => {
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />,
    );
    rerender(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[lineEvent(1, "hello world")]} version={1} />,
    );
    expect(screen.getByText("hello world")).toBeInTheDocument();
  });

  it("turns follow off when the user scrolls up", () => {
    render(<RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />);
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
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />,
    );
    rerender(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[lineEvent(1, "first")]} version={1} />,
    );
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[lineEvent(1, "first"), lineEvent(2, "second")]}
        version={2}
      />,
    );

    expect(mockScrollToIndex).toHaveBeenCalledWith(1, { align: "center" });
    const el = document.getElementById("L2");
    expect(el?.className).toMatch(/highlighted/);
  });

  it("turns follow off when a permalink resolves, so later rows do not pull the viewport to the end", () => {
    window.location.hash = "#L2";
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />,
    );
    const followBtn = screen.getByRole("button", { name: "Follow" });
    expect(followBtn).toHaveAttribute("aria-pressed", "true");
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[lineEvent(1, "first"), lineEvent(2, "second")]}
        version={1}
      />,
    );
    expect(followBtn).toHaveAttribute("aria-pressed", "false");

    mockScrollToIndex.mockClear();
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[lineEvent(1, "first"), lineEvent(2, "second"), lineEvent(3, "third")]}
        version={2}
      />,
    );
    expect(mockScrollToIndex).not.toHaveBeenCalledWith(2, { align: "end" });
  });

  it("shows the first-error control from the meta event and scrolls to it", () => {
    const meta: RunLogViewerEvent = {
      type: "meta",
      data: { runId: "r1", chunkCount: 1, byteCount: 10, droppedBytes: 0, complete: true, status: "SUCCESS", firstErrorLine: 3 },
    };
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />,
    );
    rerender(<RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[meta]} version={1} />);
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[meta, lineEvent(3, "boom: error occurred")]}
        version={2}
      />,
    );

    const btn = screen.getByRole("button", { name: "First error · line 3" });
    fireEvent.click(btn);
    expect(mockScrollToIndex).toHaveBeenCalledWith(0, { align: "center" });
  });

  it("shows a sticky-style header from a stage event and a gap row from a gap event", () => {
    const stage: RunLogViewerEvent = {
      type: "stage",
      data: { stageId: "1", name: "Build", status: "IN_PROGRESS", startOffset: 0, startLine: 1 },
    };
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />,
    );
    rerender(<RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[stage]} version={1} />);
    expect(screen.getAllByText("Build — IN_PROGRESS").length).toBeGreaterThan(0);

    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[stage, { type: "gap", data: { seq: 4, off: 100 } }]}
        version={2}
      />,
    );
    expect(screen.getByText("Missing bytes at sequence 4.")).toBeInTheDocument();
  });

  it("shows one gap row per sequence when a reconnect sends the gap again", () => {
    const gap: RunLogViewerEvent = { type: "gap", data: { seq: 4, off: 100 } };
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[gap]} version={1} />,
    );
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[gap, { type: "gap", data: { seq: 4, off: 100 } }, { type: "gap", data: { seq: 5, off: 200 } }]}
        version={3}
      />,
    );
    expect(screen.getAllByText("Missing bytes at sequence 4.")).toHaveLength(1);
    expect(screen.getByText("Missing bytes at sequence 5.")).toBeInTheDocument();
  });

  it("states the log is still being written for a stopped, unfinalized run after meta and before end", () => {
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal logFinalized={false} frames={[]} version={0} />,
    );
    // No meta event yet: the log state is not known, so no notice renders.
    expect(screen.queryByText("The log is still being written.")).not.toBeInTheDocument();

    rerender(<RunLogViewer runId="r1" runTerminal logFinalized={false} frames={[metaFrame]} version={1} />);
    expect(screen.getByText("The log is still being written.")).toBeInTheDocument();
  });

  it("shows no writing notice for a stopped run whose record reports the log finalized", () => {
    render(<RunLogViewer runId="r1" runTerminal logFinalized frames={[metaFrame, lineEvent(1, "done")]} version={1} />);
    expect(screen.getByText("done")).toBeInTheDocument();
    expect(screen.queryByText("The log is still being written.")).not.toBeInTheDocument();
  });

  it("scrolls the page to the viewer for a URL permalink", () => {
    const spy = vi.fn();
    const original = HTMLElement.prototype.scrollIntoView;
    HTMLElement.prototype.scrollIntoView = spy;
    try {
      window.location.hash = "#L2";
      render(
        <RunLogViewer
          runId="r1"
          runTerminal={false}
          logFinalized={false}
          frames={[lineEvent(1, "first"), lineEvent(2, "second")]}
          version={1}
        />,
      );
      expect(mockScrollToIndex).toHaveBeenCalledWith(1, { align: "center" });
      expect(spy).toHaveBeenCalledTimes(1);
      expect(spy).toHaveBeenCalledWith({ block: "center" });
      expect(spy.mock.contexts[0]).toBe(screen.getByLabelText("Console log"));
    } finally {
      HTMLElement.prototype.scrollIntoView = original;
    }
  });

  it("does not scroll the page for a toolbar jump to the URL permalink's own line", () => {
    const spy = vi.fn();
    const original = HTMLElement.prototype.scrollIntoView;
    HTMLElement.prototype.scrollIntoView = spy;
    try {
      window.location.hash = "#L2";
      const meta: RunLogViewerEvent = {
        type: "meta",
        data: { ...(metaFrame.data as object), firstErrorLine: 2 },
      };
      const { rerender } = render(
        <RunLogViewer runId="r1" runTerminal={false} logFinalized={false} frames={[meta]} version={1} />,
      );
      // The toolbar jump names the same line before its row arrives.
      fireEvent.click(screen.getByRole("button", { name: "First error · line 2" }));
      rerender(
        <RunLogViewer
          runId="r1"
          runTerminal={false}
          logFinalized={false}
          frames={[meta, lineEvent(1, "first"), lineEvent(2, "boom: error")]}
          version={2}
        />,
      );
      expect(mockScrollToIndex).toHaveBeenCalledWith(1, { align: "center" });
      expect(spy).not.toHaveBeenCalled();
    } finally {
      HTMLElement.prototype.scrollIntoView = original;
    }
  });

  it("does not scroll the page for a first-error jump", () => {
    const spy = vi.fn();
    const original = HTMLElement.prototype.scrollIntoView;
    HTMLElement.prototype.scrollIntoView = spy;
    try {
      const meta: RunLogViewerEvent = {
        type: "meta",
        data: { ...(metaFrame.data as object), firstErrorLine: 2 },
      };
      render(
        <RunLogViewer
          runId="r1"
          runTerminal={false}
          logFinalized={false}
          frames={[meta, lineEvent(1, "first"), lineEvent(2, "boom: error")]}
          version={1}
        />,
      );
      fireEvent.click(screen.getByRole("button", { name: "First error · line 2" }));
      expect(mockScrollToIndex).toHaveBeenCalledWith(1, { align: "center" });
      expect(spy).not.toHaveBeenCalled();
    } finally {
      HTMLElement.prototype.scrollIntoView = original;
    }
  });

  it("shows an incomplete-log banner naming the reason from the end event", () => {
    const { rerender } = render(
      <RunLogViewer runId="r1" runTerminal logFinalized={false} frames={[]} version={0} />,
    );
    rerender(
      <RunLogViewer
        runId="r1"
        runTerminal logFinalized={false}
        frames={[{ type: "end", data: { result: "FAILURE", logComplete: false, incompleteReason: "chunks lost" } }]}
        version={1}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent("chunks lost");
    expect(screen.queryByText("The log is still being written.")).not.toBeInTheDocument();
  });

  it("exposes scrollToStage on its imperative handle, for the graph's node selection", () => {
    const handleRef = createRef<RunLogViewerHandle>();
    const { rerender } = render(
      <RunLogViewer ref={handleRef} runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />,
    );
    rerender(
      <RunLogViewer
        ref={handleRef}
        runId="r1"
        runTerminal={false} logFinalized={false}
        frames={[
          { type: "stage", data: { stageId: "19", name: "Unit tests", status: "SUCCESS", startOffset: 0, startLine: 1 } },
        ]}
        version={1}
      />,
    );
    handleRef.current?.scrollToStage("19");
    expect(mockScrollToIndex).toHaveBeenCalledWith(0, { align: "start" });
  });

  it("exposes focusSearch, toggleFollow, and toggleTimestamps on its imperative handle", () => {
    const handleRef = createRef<RunLogViewerHandle>();
    render(
      <RunLogViewer ref={handleRef} runId="r1" runTerminal={false} logFinalized={false} frames={[]} version={0} />,
    );
    act(() => handleRef.current?.toggleTimestamps());
    act(() => handleRef.current?.toggleFollow());
    expect(screen.getByRole("button", { name: "Follow" })).toHaveAttribute("aria-pressed", "false");
    act(() => handleRef.current?.focusSearch());
    expect(screen.getByLabelText("Search the log")).toHaveFocus();
  });
});
