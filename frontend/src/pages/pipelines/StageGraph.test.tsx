import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { StageGraph } from "./StageGraph";
import type { RunStage } from "../../types";

function stage(overrides: Partial<RunStage> & Pick<RunStage, "stageId" | "name" | "status">): RunStage {
  return { ...overrides };
}

beforeEach(() => {
  window.location.hash = "";
});

describe("StageGraph", () => {
  it("renders two parallel branches with one nested stage as the expected node and anchor set", () => {
    const stages: RunStage[] = [
      stage({ stageId: "1", name: "Build", status: "SUCCESS", startedAt: "2026-01-01T00:00:00Z" }),
      stage({
        stageId: "2a", name: "Unit tests", status: "SUCCESS", parentId: "1", nodeType: "parallel",
        startedAt: "2026-01-01T00:01:00Z",
      }),
      stage({
        stageId: "2b", name: "Integration tests", status: "IN_PROGRESS", parentId: "1", nodeType: "parallel",
        startedAt: "2026-01-01T00:01:00Z",
      }),
      stage({
        stageId: "2b-1", name: "Setup", status: "SUCCESS", parentId: "2b", nodeType: "step",
        startedAt: "2026-01-01T00:01:05Z",
      }),
    ];

    render(<StageGraph stages={stages} terminal={false} />);

    // Every stage is an addressable anchor.
    for (const s of stages) {
      const link = document.getElementById(`node-stage-${s.stageId}`);
      expect(link).toBeTruthy();
      expect(link?.getAttribute("href")).toBe(`#stage-${s.stageId}`);
    }

    // The branch node carries a disclosure control for its nested stage.
    expect(screen.getByLabelText("Toggle nested stages of Integration tests")).toBeInTheDocument();
    // The nested stage renders in an indented list.
    expect(screen.getByLabelText("Integration tests nested stages")).toBeInTheDocument();
  });

  it("renders an unknown status with the raw value in the accessible name", () => {
    const stages: RunStage[] = [stage({ stageId: "1", name: "Deploy", status: "SOMETHING_NEW" })];
    render(<StageGraph stages={stages} terminal={false} />);
    const link = document.getElementById("node-stage-1");
    expect(link?.getAttribute("aria-label")).toContain("SOMETHING_NEW");
  });

  it("renders a started stage with the running ring", () => {
    const stages: RunStage[] = [stage({ stageId: "1", name: "Build", status: "IN_PROGRESS" })];
    const { container } = render(<StageGraph stages={stages} terminal={false} />);
    const ring = container.querySelector('[class*="ring"]');
    expect(ring).toBeInTheDocument();
  });

  it("adds a node from a stage event and replaces its status on a later event", () => {
    const stages: RunStage[] = [stage({ stageId: "1", name: "Build", status: "SUCCESS" })];
    const { rerender } = render(
      <StageGraph stages={stages} terminal={false} lastEvent={null} />,
    );
    expect(document.getElementById("node-stage-2")).toBeNull();

    rerender(
      <StageGraph
        stages={stages}
        terminal={false}
        lastEvent={{ type: "stage", data: { stageId: "2", name: "Test", status: "IN_PROGRESS", startOffset: 0, startLine: 0 } }}
      />,
    );
    const added = document.getElementById("node-stage-2");
    expect(added).toBeTruthy();
    expect(added?.getAttribute("aria-label")).toContain("In progress");

    rerender(
      <StageGraph
        stages={stages}
        terminal={false}
        lastEvent={{ type: "stage", data: { stageId: "2", name: "Test", status: "SUCCESS", startOffset: 0, startLine: 0 } }}
      />,
    );
    const updated = document.getElementById("node-stage-2");
    expect(updated?.getAttribute("aria-label")).toContain("Passed");
  });

  it("stops applying stage events once the run is terminal", () => {
    const stages: RunStage[] = [stage({ stageId: "1", name: "Build", status: "SUCCESS" })];
    const { rerender } = render(<StageGraph stages={stages} terminal lastEvent={null} />);
    rerender(
      <StageGraph
        stages={stages}
        terminal
        lastEvent={{ type: "stage", data: { stageId: "2", name: "Test", status: "IN_PROGRESS", startOffset: 0, startLine: 0 } }}
      />,
    );
    expect(document.getElementById("node-stage-2")).toBeNull();
  });

  it("calls onSelectStage when a node is selected, which the build page uses to scroll the log viewer", () => {
    const stages: RunStage[] = [stage({ stageId: "1", name: "Build", status: "SUCCESS" })];
    const onSelectStage = vi.fn();
    render(<StageGraph stages={stages} terminal={false} onSelectStage={onSelectStage} />);
    fireEvent.click(document.getElementById("node-stage-1")!);
    expect(onSelectStage).toHaveBeenCalledWith("1");
  });

  it("opens with that stage's group in view on a cold load of a #stage-<id> URL", () => {
    window.location.hash = "#stage-1";
    const stages: RunStage[] = [stage({ stageId: "1", name: "Build", status: "SUCCESS" })];
    const onSelectStage = vi.fn();
    render(<StageGraph stages={stages} terminal={false} onSelectStage={onSelectStage} />);
    expect(onSelectStage).toHaveBeenCalledWith("1");
  });

  it("renders no graph and a short line for a run with no stages", () => {
    render(<StageGraph stages={[]} terminal={false} />);
    expect(screen.getByText("This run has no stages.")).toBeInTheDocument();
  });

  it("renders the trunk only for a run with more than 200 stages, with a control to expand one bundle", () => {
    const stages: RunStage[] = [];
    for (let i = 0; i < 205; i++) {
      stages.push(stage({ stageId: `t${i}`, name: `Stage ${i}`, status: "SUCCESS", startedAt: `2026-01-01T00:${String(i % 60).padStart(2, "0")}:00Z` }));
    }
    // Give one trunk stage a child so an expand control has something to show.
    stages.push(stage({ stageId: "child-1", name: "Child", status: "SUCCESS", parentId: "t0" }));

    render(<StageGraph stages={stages} terminal={false} />);
    expect(document.getElementById("node-stage-child-1")).toBeNull();
    const expandControl = screen.getByLabelText("Expand Stage 0");
    expect(expandControl).toBeInTheDocument();

    fireEvent.click(expandControl);
    expect(document.getElementById("node-stage-child-1")).toBeTruthy();
  });
});
