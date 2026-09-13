import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import Cells from "./Cells";
import { ApiError } from "../hooks/useApi";
import type { ControllerListItem } from "../hooks/useControllers";
import type { CellPool, CellUsage } from "../types";

const mockUseCellPools = vi.fn();
const mockUseCellUsageIndex = vi.fn();
vi.mock("../hooks/useCells", () => ({
  useCellPools: () => mockUseCellPools(),
  useCellUsageIndex: (...a: unknown[]) => mockUseCellUsageIndex(...a),
  // cellUsageKey is pure, so the test uses the real rule: a stub would let
  // the page and the fixtures agree on a wrong key.
  cellUsageKey: (cluster: string, namespace: string, name: string) =>
    `${cluster}/${namespace}/${name}`,
  useCellUsageList: vi.fn(),
  useCellPool: vi.fn(),
  useCellUsage: vi.fn(),
  useCellGrants: vi.fn(),
}));

// usageIndex builds the shape useCellUsageIndex returns: rows keyed
// cluster/namespace/name, plus the set of clusters whose read answered.
function usageIndex(rows: CellUsage[], covered: string[], disabled: string[] = []) {
  const map = new Map<string, CellUsage>();
  for (const row of rows) map.set(`${row.cluster}/${row.namespace}/${row.name}`, row);
  return {
    rows: map,
    covered: new Set(covered),
    recordStoreDisabled: new Set(disabled),
    isLoading: false,
  };
}

const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
}));

const mockPatchCellPool = vi.fn();
vi.mock("../api/client", () => ({
  patchCellPool: (...a: unknown[]) => mockPatchCellPool(...a),
}));

const mockCanDoGlobal = vi.fn();
vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: (...a: unknown[]) => mockCanDoGlobal(...a),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
}));

function pool(overrides?: Partial<CellPool>): CellPool {
  return {
    name: "lts-2-555",
    spec: { profileRef: "lts-2-555", warm: 2, maxCells: 8, maxTenantsPerCell: 10 },
    status: {
      warmCells: 3,
      placedCells: 2,
      unreadyCells: 0,
      freeTenantSlots: 8,
      conditions: [{ type: "PoolReady", status: "True", reason: "Ready" }],
    },
    ...overrides,
  };
}

function cellRow(name: string, overrides?: Partial<ControllerListItem>): ControllerListItem {
  return {
    cluster: "core",
    name,
    namespace: "ns-a",
    phase: "Connected",
    endpoint: "",
    miteConnected: true,
    kind: "cell",
    cellPool: "lts-2-555",
    createdAt: "2026-09-01T00:00:00Z",
    ...overrides,
  } as ControllerListItem;
}

function usageRow(name: string, overrides?: Partial<CellUsage>): CellUsage {
  return {
    cluster: "core",
    namespace: "ns-a",
    name,
    windowStart: "2026-09-11T00:00:00Z",
    tenants: [{ tenant: "acme", runningNow: 0, runsLast24h: 2, failedLast24h: 1, jobs: 3 }],
    totals: { runningNow: 0, runsLast24h: 2, failedLast24h: 1, jobs: 3 },
    maxTenants: 10,
    freeSlots: 9,
    queueLength: 4,
    queueLengthAt: "2026-09-12T00:00:00Z",
    ...overrides,
  };
}

// Wrapped is a stable element so a rerender re-reads the mocked hooks
// instead of remounting the page.
function Wrapped() {
  return (
    <MemoryRouter>
      <Cells />
    </MemoryRouter>
  );
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={qc}>
      <Wrapped />
    </QueryClientProvider>,
  );
  const rerender = (ui: React.ReactElement) =>
    utils.rerender(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
  return { ...utils, rerender };
}

beforeEach(() => {
  vi.clearAllMocks();
  mockCanDoGlobal.mockReturnValue(true);
  mockUseCellPools.mockReturnValue({ data: [pool()], isLoading: false, error: null });
  mockUseCellUsageIndex.mockReturnValue(usageIndex([usageRow("cell-a")], ["core"]));
  mockUseControllers.mockReturnValue({
    data: [cellRow("cell-a"), cellRow("plain", { kind: "stateful", cellPool: undefined })],
    isLoading: false,
    error: null,
  });
});

describe("The pool block", () => {
  it("labels the warm cells and the free slots apart", () => {
    renderPage();
    expect(screen.getByText("Warm cells")).toBeInTheDocument();
    expect(screen.getByText("Free slots on placed cells")).toBeInTheDocument();
  });

  it("states the profile, the caps, and the PoolReady reason", () => {
    renderPage();
    expect(screen.getByText("Profile lts-2-555")).toBeInTheDocument();
    expect(screen.getByText("True (Ready)")).toBeInTheDocument();
  });

  it("states the reason for an empty pool", () => {
    mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
    mockUseCellPools.mockReturnValue({
      data: [pool({ spec: { profileRef: "lts-2-555", warm: 0, maxCells: 0, maxTenantsPerCell: 10 } })],
      isLoading: false,
      error: null,
    });
    renderPage();
    expect(screen.getByText(/warm is zero and no tenant waits/)).toBeInTheDocument();
  });

  it("names both caps when the pool cap differs from a reported cap", () => {
    mockUseCellPools.mockReturnValue({
      data: [pool({ spec: { profileRef: "lts-2-555", warm: 2, maxCells: 8, maxTenantsPerCell: 4 } })],
      isLoading: false,
      error: null,
    });
    renderPage();
    expect(screen.getByText(/sets a tenant cap of 4/)).toBeInTheDocument();
    expect(screen.getByText(/never as the smaller of two values/)).toBeInTheDocument();
  });

  it("lists a cell row that links to its Cell tab", () => {
    renderPage();
    expect(screen.getByRole("link", { name: "cell-a" })).toHaveAttribute(
      "href",
      "/controllers/core/ns-a/cell-a?tab=cell",
    );
  });

  it("lists no stateful controller", () => {
    renderPage();
    expect(screen.queryByRole("link", { name: "plain" })).not.toBeInTheDocument();
  });
});

describe("Degraded modes", () => {
  it("names no cell the caller may not read and states the rule", () => {
    // The controllers list already drops a refused cell, so the page lists
    // the readable one and names no other.
    renderPage();
    expect(screen.getByRole("link", { name: "cell-a" })).toBeInTheDocument();
    expect(screen.getAllByText(/lists the cells you may read/).length).toBeGreaterThan(0);
  });

  it("drops the four usage columns when the record store answers 503", () => {
    mockUseCellUsageIndex.mockReturnValue(usageIndex([], [], ["core"]));
    renderPage();
    expect(screen.getByText(/build record store is disabled/)).toBeInTheDocument();
    expect(screen.queryByText("Tenants over cap")).not.toBeInTheDocument();
    expect(screen.queryByText("Queue")).not.toBeInTheDocument();
    expect(screen.queryByText("Runs 24 h")).not.toBeInTheDocument();
    expect(screen.queryByText("Failures 24 h")).not.toBeInTheDocument();
    expect(screen.getByText("Age")).toBeInTheDocument();
  });

  it("renders an em dash for a remote cell's queue, never a zero", () => {
    mockUseCellUsageIndex.mockReturnValue(
      usageIndex([usageRow("cell-a", { queueLength: undefined, queueLengthAt: undefined })], ["core"]),
    );
    renderPage();
    const row = screen.getByRole("link", { name: "cell-a" }).closest("tr") as HTMLElement;
    const cells = within(row).getAllByRole("cell");
    // Column order: cell, phase, tenants over cap, queue, runs, failures.
    expect(cells[3]).toHaveTextContent("—");
    expect(cells[3]).not.toHaveTextContent("0");
  });

  it("renders skeleton rows while the reads load", () => {
    mockUseControllers.mockReturnValue({
      data: [cellRow("cell-a")],
      isLoading: true,
      error: null,
    });
    renderPage();
    expect(screen.queryByRole("link", { name: "cell-a" })).not.toBeInTheDocument();
    expect(screen.getByText("Cell")).toBeInTheDocument();
  });
});

describe("The scale control", () => {
  it("renders no control, enabled or disabled, without the update verb", () => {
    mockCanDoGlobal.mockImplementation((_p: unknown, _r: string, verb: string) => verb !== "update");
    renderPage();
    expect(screen.queryByLabelText(/Warm cells of/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Apply" })).not.toBeInTheDocument();
  });

  it("writes a growth with no confirm", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockResolvedValue(pool());
    renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "5");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(mockPatchCellPool).toHaveBeenCalledWith("lts-2-555", { spec: { warm: 5 } }),
    );
  });

  it("asks for a confirm on a drain and names the two reported numbers", async () => {
    const user = userEvent.setup();
    renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "1");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    expect(mockPatchCellPool).not.toHaveBeenCalled();
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("reports 3 warm cells");
    expect(alert).toHaveTextContent("You typed 1");
    expect(alert).toHaveTextContent(/never drains/);
  });

  it("writes the drain once the confirm is accepted", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockResolvedValue(pool());
    renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "1");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await user.click(screen.getByRole("button", { name: "Yes, drain" }));
    await waitFor(() =>
      expect(mockPatchCellPool).toHaveBeenCalledWith("lts-2-555", { spec: { warm: 1 } }),
    );
  });

  it("keeps the typed values and retries nothing on a 409", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockRejectedValue(new ApiError(409, "conflict"));
    renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "6");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(/Another writer/));
    expect(warm).toHaveValue(6);
    expect(mockPatchCellPool).toHaveBeenCalledTimes(1);
  });
});

describe("The usage join stays inside one cluster", () => {
  it("gives two clusters with the same namespace and cell name distinct counts", () => {
    mockUseCellUsageIndex.mockReturnValue(
      usageIndex(
        [
          usageRow("cell-a", { cluster: "core", totals: { runningNow: 0, runsLast24h: 7, failedLast24h: 1, jobs: 3 } }),
          usageRow("cell-a", { cluster: "member", totals: { runningNow: 0, runsLast24h: 42, failedLast24h: 9, jobs: 5 } }),
        ],
        ["core", "member"],
      ),
    );
    mockUseControllers.mockReturnValue({
      data: [cellRow("cell-a"), cellRow("cell-a", { cluster: "member" })],
      isLoading: false,
      error: null,
    });
    renderPage();
    const rows = screen.getAllByRole("link", { name: "cell-a" });
    expect(rows).toHaveLength(2);
    const core = rows[0].closest("tr") as HTMLElement;
    const member = rows[1].closest("tr") as HTMLElement;
    // Column order: cell, phase, tenants over cap, queue, runs, failures.
    expect(within(core).getAllByRole("cell")[4]).toHaveTextContent("7");
    expect(within(member).getAllByRole("cell")[4]).toHaveTextContent("42");
  });

  it("requests the usage read for each cluster the list names", () => {
    mockUseControllers.mockReturnValue({
      data: [cellRow("cell-a"), cellRow("cell-b", { cluster: "member" })],
      isLoading: false,
      error: null,
    });
    renderPage();
    expect(mockUseCellUsageIndex).toHaveBeenCalledWith(["core", "member"]);
  });

  it("states that usage is not available for a cluster the read did not cover", () => {
    mockUseCellUsageIndex.mockReturnValue(usageIndex([usageRow("cell-a")], ["core"]));
    mockUseControllers.mockReturnValue({
      data: [cellRow("cell-a"), cellRow("cell-b", { cluster: "member" })],
      isLoading: false,
      error: null,
    });
    renderPage();
    const row = screen.getByRole("link", { name: "cell-b" }).closest("tr") as HTMLElement;
    const cells = within(row).getAllByRole("cell");
    expect(cells[2]).toHaveTextContent("not available");
    // A covered row still reports its own numbers.
    const covered = screen.getByRole("link", { name: "cell-a" }).closest("tr") as HTMLElement;
    expect(within(covered).getAllByRole("cell")[2]).toHaveTextContent("1 / 10");
  });

  it("renders no count for a cell whose cluster carries another cluster's row", () => {
    // The member cell has no usage row of its own. A namespace-and-name
    // index would hand it the core row instead.
    mockUseCellUsageIndex.mockReturnValue(usageIndex([usageRow("cell-a")], ["core", "member"]));
    mockUseControllers.mockReturnValue({
      data: [cellRow("cell-a", { cluster: "member" })],
      isLoading: false,
      error: null,
    });
    renderPage();
    const row = screen.getByRole("link", { name: "cell-a" }).closest("tr") as HTMLElement;
    const cells = within(row).getAllByRole("cell");
    expect(cells[2]).toHaveTextContent("—");
    expect(cells[4]).toHaveTextContent("—");
  });
});

describe("The scale control sends one dial", () => {
  it("sends only maxCells when only maxCells changed", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockResolvedValue(pool());
    renderPage();
    const maxCells = screen.getByLabelText("Max cells of lts-2-555");
    await user.clear(maxCells);
    await user.type(maxCells, "12");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(mockPatchCellPool).toHaveBeenCalledWith("lts-2-555", { spec: { maxCells: 12 } }),
    );
  });

  it("sends both dials when both changed", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockResolvedValue(pool());
    renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    const maxCells = screen.getByLabelText("Max cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "4");
    await user.clear(maxCells);
    await user.type(maxCells, "12");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(mockPatchCellPool).toHaveBeenCalledWith("lts-2-555", {
        spec: { warm: 4, maxCells: 12 },
      }),
    );
  });

  it("sends nothing when no dial changed", async () => {
    const user = userEvent.setup();
    renderPage();
    await user.click(screen.getByRole("button", { name: "Apply" }));
    expect(mockPatchCellPool).not.toHaveBeenCalled();
  });
});

describe("The scale control tracks what the user edited", () => {
  it("sends warm alone when a poll changes maxCells under an untouched input", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockResolvedValue(pool());
    const { rerender } = renderPage();

    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "5");

    // A poll lands: another writer raised maxCells from 8 to 20. The user
    // never touched that input, so the write must not carry it.
    mockUseCellPools.mockReturnValue({
      data: [pool({ spec: { profileRef: "lts-2-555", warm: 2, maxCells: 20, maxTenantsPerCell: 10 } })],
      isLoading: false,
      error: null,
    });
    rerender(<Wrapped />);

    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(mockPatchCellPool).toHaveBeenCalledWith("lts-2-555", { spec: { warm: 5 } }),
    );
  });

  it("re-seeds an untouched input from the live spec", async () => {
    const user = userEvent.setup();
    const { rerender } = renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "5");

    mockUseCellPools.mockReturnValue({
      data: [pool({ spec: { profileRef: "lts-2-555", warm: 2, maxCells: 20, maxTenantsPerCell: 10 } })],
      isLoading: false,
      error: null,
    });
    rerender(<Wrapped />);

    // The untouched input shows the new stored value; the edited one keeps
    // what the user typed.
    expect(screen.getByLabelText("Max cells of lts-2-555")).toHaveValue(20);
    expect(screen.getByLabelText("Warm cells of lts-2-555")).toHaveValue(5);
  });

  it("sends nothing when only the live spec moved", async () => {
    const user = userEvent.setup();
    const { rerender } = renderPage();
    mockUseCellPools.mockReturnValue({
      data: [pool({ spec: { profileRef: "lts-2-555", warm: 6, maxCells: 8, maxTenantsPerCell: 10 } })],
      isLoading: false,
      error: null,
    });
    rerender(<Wrapped />);
    await user.click(screen.getByRole("button", { name: "Apply" }));
    expect(mockPatchCellPool).not.toHaveBeenCalled();
  });
});

describe("The scale control reports every refusal", () => {
  it("renders the message of a 400", async () => {
    const user = userEvent.setup();
    // The client puts the fixed status copy in message and the server's
    // text in the body, as errorFromResponse does.
    mockPatchCellPool.mockRejectedValue(
      new ApiError(400, "The request could not be completed.", {
        error: "spec.maxCells must be 0 or at least spec.warm",
      }),
    );
    renderPage();
    const maxCells = screen.getByLabelText("Max cells of lts-2-555");
    await user.clear(maxCells);
    await user.type(maxCells, "1");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        "spec.maxCells must be 0 or at least spec.warm",
      ),
    );
  });

  it("keeps the conflict wording for a 409", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockRejectedValue(new ApiError(409, "conflict"));
    renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "6");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(/Another writer/));
  });

  it("clears a refusal on the next apply", async () => {
    const user = userEvent.setup();
    mockPatchCellPool.mockRejectedValueOnce(
      new ApiError(400, "The request could not be completed.", { error: "bad dial" }),
    );
    renderPage();
    const warm = screen.getByLabelText("Warm cells of lts-2-555");
    await user.clear(warm);
    await user.type(warm, "6");
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("bad dial"));

    mockPatchCellPool.mockResolvedValue(pool());
    await user.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() => expect(screen.queryByText("bad dial")).not.toBeInTheDocument());
  });
});
