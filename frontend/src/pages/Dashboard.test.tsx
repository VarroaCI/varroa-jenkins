import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import Dashboard from "./Dashboard";
import { renderWithProviders } from "../test/render-utils";
import type { Permissions } from "../types/auth";
import { createControllerListItem } from "../test/factories";
import { failingJob, overview, tenantRow } from "../test/overviewFactories";

const mockUseAuth = vi.fn();
vi.mock("../context/AuthContext", () => ({
  useAuth: () => mockUseAuth(),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

const mockUseControllers = vi.fn();
const mockFanoutFailures = vi.fn((): { name: string; ok: boolean; error?: string }[] => []);
vi.mock("../hooks/useControllers", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useControllers")>()),
  useControllers: (enabled?: boolean) => mockUseControllers(enabled),
  useControllerFanoutFailures: () => ({ data: mockFanoutFailures() }),
}));

const mockUseClusters = vi.fn();
vi.mock("../hooks/useClusters", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useClusters")>()),
  useClusters: (enabled?: boolean) => mockUseClusters(enabled),
}));

vi.mock("../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useApi")>()),
  bffFetch: vi.fn().mockResolvedValue({ items: [] }),
}));

const mockGetPipelinesOverview = vi.fn();
const mockUseEventStream = vi.fn((_url: string | null, _scope: string | null) => ({
  lastEvent: null,
  readyState: "closed",
  error: null,
}));
vi.mock("../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/client")>()),
  getPipelinesOverview: (...args: unknown[]) => mockGetPipelinesOverview(...args),
  getUpdateCenterStatus: vi.fn().mockResolvedValue({ enabled: false, conditions: [], gaps: [], unservableCount: 0 }),
  fleetRunStreamUrl: () => "/api/v1/pipelines/runs/stream",
}));

vi.mock("../hooks/useEventStream", () => ({
  useEventStream: (url: string | null, scope: string | null) => mockUseEventStream(url, scope),
}));

vi.mock("./ConnectClientTile", async () => {
  const { createElement } = await import("react");
  return {
    ConnectClientTile: () => createElement("div", { "data-testid": "connect-client-tile" }),
  };
});

const CONTROLLERS: Permissions = { global: { controllers: { read: true } }, scopes: [] };
const BOTH: Permissions = { global: { pipelines: { read: true }, controllers: { read: true } }, scopes: [] };
const NEITHER: Permissions = { global: {}, scopes: [] };
const CREATOR: Permissions = { global: { pipelines: { read: true }, controllers: { read: true, create: true } }, scopes: [] };

function auth(over: Partial<Record<string, unknown>> = {}) {
  return {
    permissions: BOTH,
    permissionsLoading: false,
    permissionsError: null,
    retryPermissions: vi.fn(),
    ...over,
  };
}

describe("Dashboard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseAuth.mockReturnValue(auth());
    mockUseControllers.mockReturnValue({ data: [], isPending: false, isError: false, error: null, refetch: vi.fn() });
    mockUseClusters.mockReturnValue({ data: [], isPending: false, isError: false, error: null });
    mockFanoutFailures.mockReturnValue([]);
    mockGetPipelinesOverview.mockResolvedValue(overview({}, [tenantRow("pay")]));
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("titles the page Overview", () => {
    renderWithProviders(<Dashboard />);
    expect(screen.getByRole("heading", { level: 1, name: "Overview" })).toBeInTheDocument();
  });

  it("explains a user with neither access", () => {
    mockUseAuth.mockReturnValue(auth({ permissions: NEITHER }));
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("No access")).toBeInTheDocument();
    expect(
      screen.getByText("No role gives you access to pipelines or controllers. Ask an administrator to add you."),
    ).toBeInTheDocument();
    expect(mockGetPipelinesOverview).not.toHaveBeenCalled();
  });

  it("shows Retry when the permissions read fails", async () => {
    const retry = vi.fn();
    mockUseAuth.mockReturnValue(auth({ permissions: undefined, permissionsError: new Error("boom"), retryPermissions: retry }));
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("Unable to load your access")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledTimes(1);
  });

  it("renders the attention panel, the day panel, and the feed in that order", async () => {
    mockGetPipelinesOverview.mockResolvedValue(overview({ failingJobs: [failingJob()] }, [tenantRow("payments")]));
    renderWithProviders(<Dashboard />);
    const attention = await screen.findByRole("heading", { name: /Needs attention/ });
    const day = screen.getByRole("heading", { name: "Last 24 hours" });
    const feed = screen.getByRole("heading", { name: "Earlier today" });
    expect(attention.compareDocumentPosition(day) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(day.compareDocumentPosition(feed) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("shows New controller only with the create verb", () => {
    renderWithProviders(<Dashboard />);
    expect(screen.queryByRole("link", { name: "New controller" })).toBeNull();
    mockUseAuth.mockReturnValue(auth({ permissions: CREATOR }));
    renderWithProviders(<Dashboard />);
    expect(screen.getByRole("link", { name: "New controller" })).toHaveAttribute("href", "/controllers/create");
  });

  it("renders the connect tile once, after the feed", async () => {
    renderWithProviders(<Dashboard />);
    const tile = await screen.findByTestId("connect-client-tile");
    const feed = await screen.findByRole("heading", { name: "Earlier today" });
    expect(feed.compareDocumentPosition(tile) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getAllByTestId("connect-client-tile")).toHaveLength(1);
  });

  it("names a cluster whose list failed for a controller reader", async () => {
    mockFanoutFailures.mockReturnValue([{ name: "member-b", ok: false, error: "cluster unreachable" }]);
    renderWithProviders(<Dashboard />);
    const notice = await screen.findByRole("status");
    expect(within(notice).getByText(/member-b/)).toBeInTheDocument();
  });

  it("a render error in the day panel keeps the other parts", async () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    mockGetPipelinesOverview.mockResolvedValue({
      ...overview({ waitingInputs: [] }, [tenantRow("pay")]),
      totals: undefined,
    });
    mockUseControllers.mockReturnValue({
      data: [createControllerListItem({ name: "keeper", attention: { kind: "failed" } })],
      isPending: false,
      isError: false,
      error: null,
      refetch: vi.fn(),
    });
    renderWithProviders(<Dashboard />);
    expect(await screen.findByText("keeper")).toBeInTheDocument();
    expect(await screen.findByText("Something went wrong")).toBeInTheDocument();
    spy.mockRestore();
  });

  it("opens no run stream for a controllers-only user", async () => {
    mockUseAuth.mockReturnValue(auth({ permissions: CONTROLLERS }));
    renderWithProviders(<Dashboard />);
    await screen.findByRole("heading", { name: "Last 24 hours" });
    expect(mockGetPipelinesOverview).not.toHaveBeenCalled();
    expect(mockUseClusters).toHaveBeenLastCalledWith(true);
    const streamUrls = mockUseEventStream.mock.calls.map((c) => c[0]);
    expect(streamUrls.every((u) => u === null)).toBe(true);
  });
});
