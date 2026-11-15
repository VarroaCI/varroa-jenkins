import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AttentionPanel } from "./AttentionPanel";
import { renderWithProviders } from "../../test/render-utils";
import { createControllerListItem } from "../../test/factories";
import { clusterEntry, failingJob, overview, waitingInput } from "../../test/overviewFactories";
import type { ControllerListItem } from "../../hooks/useControllers";
import type { ClusterEntry, PipelinesOverview } from "../../types";

const mocks = vi.hoisted(() => ({
  getPipelinesOverview: vi.fn(),
  proceedRunInput: vi.fn(),
  abortRunInput: vi.fn(),
  rerunRun: vi.fn(),
  getUpdateCenterStatus: vi.fn(),
}));
vi.mock("../../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/client")>()),
  ...mocks,
}));

const mockUseControllers = vi.fn();
vi.mock("../../hooks/useControllers", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../hooks/useControllers")>()),
  useControllers: (enabled?: boolean) => mockUseControllers(enabled),
}));

const mockUseClusters = vi.fn();
vi.mock("../../hooks/useClusters", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../hooks/useClusters")>()),
  useClusters: (enabled?: boolean) => mockUseClusters(enabled),
}));

vi.mock("../../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../hooks/useApi")>()),
  bffFetch: vi.fn().mockResolvedValue({ items: [] }),
}));

function controllersReturn(data: ControllerListItem[] | undefined) {
  return { data, isPending: false, isError: false, error: null, refetch: vi.fn() };
}

function clustersReturn(data: ClusterEntry[]) {
  return { data, isPending: false, isError: false, error: null, refetch: vi.fn() };
}

function renderPanel(props: { canPipelines?: boolean; canControllers?: boolean; isAdmin?: boolean } = {}) {
  return renderWithProviders(
    <AttentionPanel
      canPipelines={props.canPipelines ?? true}
      canControllers={props.canControllers ?? true}
      isAdmin={props.isAdmin ?? false}
    />,
  );
}

function setOverview(ov: PipelinesOverview) {
  mocks.getPipelinesOverview.mockResolvedValue(ov);
}

describe("AttentionPanel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseControllers.mockReturnValue(controllersReturn([]));
    mockUseClusters.mockReturnValue(clustersReturn([]));
    mocks.getUpdateCenterStatus.mockResolvedValue({ enabled: false, conditions: [], gaps: [], unservableCount: 0 });
    mocks.proceedRunInput.mockResolvedValue({ state: "requested" });
    mocks.abortRunInput.mockResolvedValue({ state: "requested" });
    mocks.rerunRun.mockResolvedValue({ runId: "run-212", intentKey: "k", state: "requested" });
    setOverview(overview());
  });

  it("renders nothing when no item needs attention", async () => {
    renderPanel();
    await waitFor(() => expect(mocks.getPipelinesOverview).toHaveBeenCalled());
    expect(screen.queryByRole("heading", { name: /Needs attention/ })).toBeNull();
  });

  it("shows the count and the breakdown", async () => {
    setOverview(overview({ waitingInputs: [waitingInput()], failingJobs: [failingJob()] }));
    renderPanel();
    expect(await screen.findByRole("heading", { name: "Needs attention 2" })).toBeInTheDocument();
    expect(screen.getByText("1 approval · 1 failing job")).toBeInTheDocument();
  });

  it("Proceed posts the first input with no parameters", async () => {
    setOverview(overview({ waitingInputs: [waitingInput()] }));
    renderPanel();
    await userEvent.click(await screen.findByRole("button", { name: "Proceed" }));
    await waitFor(() => expect(mocks.proceedRunInput).toHaveBeenCalledWith("run-41", "promote", {}));
  });

  it("Abort posts the abort route", async () => {
    setOverview(overview({ waitingInputs: [waitingInput()] }));
    renderPanel();
    await userEvent.click(await screen.findByRole("button", { name: "Abort" }));
    await waitFor(() => expect(mocks.abortRunInput).toHaveBeenCalledWith("run-41", "promote"));
  });

  it("hides Proceed and Abort for a reader without write", async () => {
    setOverview(overview({ waitingInputs: [waitingInput({}, { canWrite: false })] }));
    renderPanel();
    expect(await screen.findByRole("link", { name: "Open" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Proceed" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Abort" })).toBeNull();
  });

  it("an input with parameters shows Open only", async () => {
    setOverview(overview({ waitingInputs: [waitingInput({ input: { id: "p", parameterCount: 2 } })] }));
    renderPanel();
    expect(await screen.findByText(/needs 2 values/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Proceed" })).toBeNull();
    expect(screen.getByRole("link", { name: "Open" })).toHaveAttribute("href", "/pipelines/blockctl/deploy/run-41");
  });

  it("Run again posts the rerun route for the failed run", async () => {
    setOverview(overview({ failingJobs: [failingJob()] }));
    renderPanel();
    await userEvent.click(await screen.findByRole("button", { name: "Run again" }));
    await waitFor(() => expect(mocks.rerunRun).toHaveBeenCalledWith("run-212"));
  });

  it("hides Run again without write", async () => {
    setOverview(overview({ failingJobs: [failingJob({}, { canWrite: false })] }));
    renderPanel();
    expect(await screen.findByRole("link", { name: "Open log" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Run again" })).toBeNull();
  });

  it("Open log links the build page", async () => {
    setOverview(overview({ failingJobs: [failingJob()] }));
    renderPanel();
    expect(await screen.findByRole("link", { name: "Open log" })).toHaveAttribute("href", "/pipelines/payments/release/run-212");
  });

  it("a controller row renders its StatusPill and links the controller page", async () => {
    mockUseControllers.mockReturnValue(
      controllersReturn([
        createControllerListItem({ name: "ctrl1", namespace: "varroa", phase: "Failed", attention: { kind: "failed", message: "boom" } }),
      ]),
    );
    renderPanel();
    const row = (await screen.findByText("ctrl1")).closest("li") as HTMLElement;
    expect(within(row).getAllByText("Failed")).toHaveLength(2);
    expect(within(row).getByRole("link", { name: "Open" })).toHaveAttribute("href", "/controllers/core/varroa/ctrl1");
  });

  it("shows eight rows and Show all expands", async () => {
    const many = Array.from({ length: 10 }, (_, i) => failingJob({}, { runId: `f${i}`, createdAt: `2026-11-14T0${i % 9}:00:00Z` }));
    setOverview(overview({ failingJobs: many }));
    renderPanel();
    await screen.findByRole("heading", { name: "Needs attention 10" });
    expect(screen.getAllByRole("listitem")).toHaveLength(8);
    await userEvent.click(screen.getByRole("button", { name: "Show all 10" }));
    expect(screen.getAllByRole("listitem")).toHaveLength(10);
    expect(screen.getByRole("button", { name: "Show fewer" })).toBeInTheDocument();
  });

  it("names more failing jobs on the Pipelines page", async () => {
    setOverview(overview({ failingJobs: [failingJob()], moreFailingJobs: true }));
    renderPanel();
    const link = await screen.findByRole("link", { name: "More failing jobs are on the Pipelines page." });
    expect(link).toHaveAttribute("href", "/pipelines?result=FAILURE");
    expect(screen.getByRole("heading", { name: "Needs attention 1+" })).toBeInTheDocument();
  });

  it("a pipelines-only user reads no controllers", async () => {
    setOverview(overview({ failingJobs: [failingJob()] }));
    renderPanel({ canControllers: false });
    await screen.findByRole("heading", { name: "Needs attention 1" });
    expect(mockUseControllers).toHaveBeenCalledWith(false);
    expect(mockUseClusters).toHaveBeenCalledWith(false);
  });

  it("an overview error keeps the controller rows", async () => {
    mocks.getPipelinesOverview.mockRejectedValue(new Error("boom"));
    mockUseControllers.mockReturnValue(
      controllersReturn([createControllerListItem({ name: "ctrl2", attention: { kind: "bootFailed" } })]),
    );
    renderPanel();
    expect(await screen.findByText("ctrl2")).toBeInTheDocument();
  });

  it("names a stopped member cluster for a controller reader", async () => {
    mockUseClusters.mockReturnValue(clustersReturn([clusterEntry({ name: "member-b", healthy: false })]));
    renderPanel();
    expect(await screen.findByText("Cluster member-b stopped reporting")).toBeInTheDocument();
  });

  it("shows the update center row only to an administrator", async () => {
    mocks.getUpdateCenterStatus.mockResolvedValue({ enabled: true, conditions: [], gaps: [], unservableCount: 2 });
    renderPanel({ isAdmin: true });
    expect(await screen.findByText("The update center cannot serve 2 plugins")).toBeInTheDocument();
  });

  it("does not read the update center for a non-administrator", async () => {
    renderPanel({ isAdmin: false });
    await waitFor(() => expect(mocks.getPipelinesOverview).toHaveBeenCalled());
    expect(mocks.getUpdateCenterStatus).not.toHaveBeenCalled();
  });
});
