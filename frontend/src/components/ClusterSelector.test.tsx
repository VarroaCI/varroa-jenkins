import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ClusterEntry } from "../types";
import ClusterSelector from "./ClusterSelector";

// Mutable data so each test can shape the cluster set. coreOf stays a real-ish
// impl so the seed/label logic is exercised, not stubbed away.
const mockClusters: { data: ClusterEntry[] | undefined } = { data: undefined };
vi.mock("../hooks/useClusters", () => ({
  useClusters: () => mockClusters,
  coreOf: (c: ClusterEntry[] | undefined) => c?.find((x) => x.core),
}));

const cluster = (name: string, over: Partial<ClusterEntry> = {}): ClusterEntry => ({
  name,
  healthy: true,
  core: false,
  lastHeartbeat: "2025-01-01T00:00:00Z",
  operatorVersion: "1.0",
  k8sVersion: "1.28",
  controllerCount: 0,
  connectedCount: 0,
  state: "active",
  ...over,
});

describe("ClusterSelector", () => {
  beforeEach(() => {
    mockClusters.data = undefined;
  });

  it("renders a labeled option per healthy/active cluster, core labeled '(core)'", () => {
    mockClusters.data = [cluster("main", { core: true }), cluster("dev-cluster")];
    render(<ClusterSelector value="main" onChange={vi.fn()} />);

    const select = screen.getByRole("combobox") as HTMLSelectElement;
    const options = Array.from(select.querySelectorAll("option"));
    expect(options.map((o) => o.value)).toEqual(["main", "dev-cluster"]);
    expect(options[0].textContent).toMatch(/main.*core/i);
    expect(options[1].textContent).toBe("dev-cluster");
  });

  it("hides itself when there are fewer than two active clusters", () => {
    mockClusters.data = [cluster("main", { core: true })];
    const { container } = render(<ClusterSelector value="main" onChange={vi.fn()} />);
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing (no crash) when clusters have not loaded", () => {
    mockClusters.data = undefined;
    const { container } = render(<ClusterSelector value="core" onChange={vi.fn()} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("excludes unhealthy and draining/drained clusters", () => {
    mockClusters.data = [
      cluster("main", { core: true }),
      cluster("dev-cluster"),
      cluster("edge", { healthy: false }),
      cluster("gone", { state: "drained" }),
    ];
    render(<ClusterSelector value="main" onChange={vi.fn()} />);
    const values = Array.from(screen.getByRole("combobox").querySelectorAll("option")).map(
      (o) => (o as HTMLOptionElement).value,
    );
    expect(values).toEqual(["main", "dev-cluster"]);
  });

  it("auto-seeds the value to the core when the current selection is not an active cluster", () => {
    mockClusters.data = [cluster("main", { core: true }), cluster("dev-cluster")];
    const onChange = vi.fn();
    // Placeholder "core" is not among the active cluster names → seed to the core.
    render(<ClusterSelector value="core" onChange={onChange} />);
    expect(onChange).toHaveBeenCalledWith("main");
  });

  it("does not re-seed when the value already matches an active cluster", () => {
    mockClusters.data = [cluster("main", { core: true }), cluster("dev-cluster")];
    const onChange = vi.fn();
    render(<ClusterSelector value="dev-cluster" onChange={onChange} />);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("emits the picked cluster on change", async () => {
    mockClusters.data = [cluster("main", { core: true }), cluster("dev-cluster")];
    const onChange = vi.fn();
    render(<ClusterSelector value="main" onChange={onChange} />);
    await userEvent.selectOptions(screen.getByRole("combobox"), "dev-cluster");
    expect(onChange).toHaveBeenCalledWith("dev-cluster");
  });
});
