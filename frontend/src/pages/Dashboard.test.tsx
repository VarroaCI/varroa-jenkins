import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import Dashboard from "./Dashboard";
import { renderWithProviders } from "../test/render-utils";
import { createControllerListItem } from "../test/factories";

const mockUseControllers = vi.fn();
const mockFanoutFailures = vi.fn((): { name: string; ok: boolean; error?: string }[] => []);
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
  useControllerFanoutFailures: () => ({ data: mockFanoutFailures() }),
}));

const mockUseClusters = vi.fn(() => ({ data: undefined, isLoading: false, error: null }));
vi.mock("../hooks/useClusters", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useClusters")>()),
  useClusters: () => mockUseClusters(),
}));

vi.mock("../hooks/useApi", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../hooks/useApi")>()),
  bffFetch: vi.fn().mockResolvedValue({items: []}),
}));

vi.mock("./ConnectClientTile", async () => {
  const { createElement } = await import("react");
  return {
    ConnectClientTile: () => createElement("div", { "data-testid": "connect-client-tile" }),
  };
});

import * as useApi from "../hooks/useApi";

describe("Dashboard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe("Loading state", () => {
    it("shows skeleton metric cards when loading", () => {
      mockUseControllers.mockReturnValue({
        data: null,
        isLoading: true,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("Brood overview")).toBeInTheDocument();
      expect(screen.getByText("Loading...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows an error banner when loading fails", () => {
      mockUseControllers.mockReturnValue({
        data: null,
        isLoading: false,
        error: { message: "Connection refused" },
      });
      renderWithProviders(<Dashboard />);

      expect(
        screen.getByText(/Failed to load brood data: Connection refused/),
      ).toBeInTheDocument();
    });
  });

  describe("Empty state", () => {
    it('shows "No controllers deployed yet" when there are no controllers', () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(
        screen.getByText(/No controllers deployed yet/),
      ).toBeInTheDocument();
    });
  });

  describe("Happy path", () => {
    it("renders the connect tile after the metric cards", () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      const metric = screen.getByText("Total controllers");
      const tile = screen.getByTestId("connect-client-tile");
      expect(metric.compareDocumentPosition(tile) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("renders the page heading and description with total count", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-b", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("Brood overview")).toBeInTheDocument();
      expect(screen.getByText(/2 controllers across 1 namespace$/)).toBeInTheDocument();
    });

    it("counts (cluster, namespace) identities, not bare namespace names", () => {
      mockUseControllers.mockReturnValue({
        data: [
          // Same namespace name in two clusters must count as two.
          { name: "ctrl-a", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-b", cluster: "cluster-b", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-c", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText(/3 controllers across 2 namespaces/)).toBeInTheDocument();
    });

    it("uses singular nouns for a single controller in a single namespace", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText(/1 controller across 1 namespace$/)).toBeInTheDocument();
    });

    it("no longer advertises an unwired operator uptime", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "varroa", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.queryByText(/operator uptime/)).not.toBeInTheDocument();
      expect(screen.queryByText(/added this week/)).not.toBeInTheDocument();
    });

    it("renders metric cards: Total, Connected, Provisioning, Needs attention", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
          { name: "ctrl-b", cluster: "core", namespace: "default", phase: "Provisioning", miteConnected: false, endpoint: "" },
          { name: "ctrl-c", cluster: "core", namespace: "default", phase: "Failed", miteConnected: false, endpoint: "" },
          { name: "ctrl-d", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("Total controllers")).toBeInTheDocument();
      expect(screen.getByText("Mites connected")).toBeInTheDocument();
      // "Provisioning" appears in both MetricCard label and StatusPill
      expect(screen.getAllByText("Provisioning").length).toBeGreaterThanOrEqual(1);
      expect(screen.getByText("Needs attention")).toBeInTheDocument();
      expect(screen.getByText("4")).toBeInTheDocument(); // total
      expect(screen.getByText("2 / 4")).toBeInTheDocument(); // connected / total
    });

    it("shows correct count for provisioning", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Provisioning", miteConnected: false, endpoint: "" },
          { name: "ctrl-b", cluster: "core", namespace: "default", phase: "Provisioning", miteConnected: false, endpoint: "" },
          { name: "ctrl-c", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("2 in progress")).toBeInTheDocument();
    });

    it('shows "all clear" when there are no failed controllers', () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText("all clear")).toBeInTheDocument();
    });

    it("renders the brood health card with controller names and links", () => {
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running", miteConnected: true, endpoint: "" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.getByText(/Brood health/)).toBeInTheDocument();
      const link = screen.getByRole("link", { name: /ctrl-a/i });
      expect(link).toBeInTheDocument();
      expect(link).toHaveAttribute("href", "/controllers/core/default/ctrl-a");
    });

    it("renders real controller state instead of a synthetic history strip", () => {
      mockUseControllers.mockReturnValue({
        data: [
          createControllerListItem({
            name: "alpha",
            phase: "Connected",
            miteConnected: true,
            jenkinsHealth: "healthy",
            jenkinsVersion: "2.492.3",
            lastSeen: new Date(Date.now() - 30_000).toISOString(),
          }),
          createControllerListItem({
            name: "beta",
            phase: "Provisioning",
            miteConnected: false,
            attention: { kind: "bootFailed", message: "exit 5" },
          }),
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      expect(screen.queryByText(/last 24h/)).toBeNull();
      expect(screen.getByText("2 controllers")).toBeInTheDocument();

      const alpha = screen.getByText("alpha").closest<HTMLElement>("[data-testid='health-row']")!;
      // age() renders "just now" under a minute and "Nm ago"/"Nh ago" beyond.
      expect(within(alpha).getByText(/^seen (just now|.* ago)$/)).toBeInTheDocument();
      expect(within(alpha).getByText("healthy")).toBeInTheDocument();
      expect(within(alpha).getByText("Jenkins 2.492.3")).toBeInTheDocument();

      const beta = screen.getByText("beta").closest<HTMLElement>("[data-testid='health-row']")!;
      expect(within(beta).getByText("Boot failed")).toBeInTheDocument();
      expect(within(beta).getByText(/never seen/)).toBeInTheDocument();

      // No synthetic strip: Pulse contributes one <i>; the 48-bar strip is gone.
      expect(alpha.querySelector("[class*='healthStrip']")).toBeNull();
      expect(alpha.querySelectorAll("i").length).toBeLessThanOrEqual(1);
    });

    it("renders the 'New controller' link", () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<Dashboard />);

      const newCtrlLink = screen.getByText(/New controller/);
      expect(newCtrlLink).toBeInTheDocument();
      expect(newCtrlLink.closest("a")).toHaveAttribute("href", "/controllers/create");
    });
  });

  describe("Activity feed", () => {
    it("shows 'No activity yet' when events are empty", async () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      const { queryClient } = renderWithProviders(<Dashboard />);

      // Pre-set the activity query data
      queryClient.setQueryData(["activity"], []);

      await waitFor(() => {
        expect(
          screen.getByText(/No activity yet/),
        ).toBeInTheDocument();
      });
    });

    it("renders a mapped icon for an auth.local.login_failed event", async () => {
      mockUseControllers.mockReturnValue({
        data: [],
        isLoading: false,
        error: null,
      });
      const authEvent = {
        timestamp: new Date().toISOString(),
        type: "auth.local.login_failed",
        source: "user",
        controller: "",
        namespace: "",
        message: "Sign-in failed",
      };
      // The feed refetches on mount, so the mocked /activity path must serve the event.
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) =>
        Promise.resolve(path.startsWith("/activity") ? { items: [authEvent] } : { items: [] }),
      );
      try {
        renderWithProviders(<Dashboard />);
        await waitFor(() => {
          expect(screen.getByText("⚠")).toBeInTheDocument();
        });
        expect(screen.queryByText("·")).not.toBeInTheDocument();
      } finally {
        vi.mocked(useApi.bffFetch).mockResolvedValue({ items: [] });
      }
    });
  });

  describe("Recent activity rows", () => {
    // serveActivity answers the feed's /activity read with these events.
    function serveActivity(items: unknown[]) {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) =>
        Promise.resolve(path.startsWith("/activity") ? { items } : { items: [] }),
      );
    }

    afterEach(() => {
      vi.mocked(useApi.bffFetch).mockResolvedValue({ items: [] });
    });

    it("asks the server to leave out routine sign-ins", async () => {
      mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
      serveActivity([
        { timestamp: new Date().toISOString(), type: "pipeline.run_requested", source: "user", message: "started build 4", actor: "local:bob" },
      ]);
      renderWithProviders(<Dashboard />);

      expect(await screen.findByText("started build 4")).toBeInTheDocument();
      expect(useApi.bffFetch).toHaveBeenCalledWith("/activity?limit=5&excludeRoutineSignIns=true");
    });

    it("says the record store did not answer, and Retry asks again", async () => {
      mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) =>
        path.startsWith("/activity")
          ? Promise.reject(new useApi.ApiError(503, "record store unreachable", { error: "record store unreachable", code: "record_store_unreachable" }))
          : Promise.resolve({ items: [] }),
      );
      renderWithProviders(<Dashboard />);

      expect(
        await screen.findByText("The build record store did not answer, so activity is not available."),
      ).toBeInTheDocument();
      expect(screen.queryByText(/No activity yet/)).not.toBeInTheDocument();

      const before = vi.mocked(useApi.bffFetch).mock.calls.filter(([p]) => String(p).startsWith("/activity")).length;
      await userEvent.click(screen.getByRole("button", { name: "Retry" }));
      await waitFor(() => {
        const after = vi.mocked(useApi.bffFetch).mock.calls.filter(([p]) => String(p).startsWith("/activity")).length;
        expect(after).toBeGreaterThan(before);
      });
    });

    it("shows the actor, not 'system', for an event with no controller", async () => {
      mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
      serveActivity([
        { timestamp: new Date().toISOString(), type: "pipeline.run_requested", source: "user", message: "deployed X", actor: "local:bob" },
      ]);
      renderWithProviders(<Dashboard />);

      expect(await screen.findByText("local:bob")).toBeInTheDocument();
      expect(screen.queryByText("system")).not.toBeInTheDocument();
    });

    it("drops the bold prefix when the message names the actor", async () => {
      mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
      serveActivity([
        { timestamp: new Date().toISOString(), type: "pipeline.run_requested", source: "user", message: "local:bob changed a setting", actor: "local:bob" },
      ]);
      const { container } = renderWithProviders(<Dashboard />);

      expect(await screen.findByText("local:bob changed a setting")).toBeInTheDocument();
      expect(container.querySelector("b")).toBeNull();
    });

    it("renders a relative age with the full date in its title", async () => {
      mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
      const ts = new Date().toISOString();
      serveActivity([
        { timestamp: ts, type: "pipeline.run_requested", source: "user", message: "deployed X", actor: "local:bob" },
      ]);
      renderWithProviders(<Dashboard />);

      const age = await screen.findByTitle(new Date(ts).toLocaleString());
      expect(age).toHaveTextContent("just now");
    });
  });

  describe("Update Center gaps chip", () => {
    afterEach(() => {
      vi.mocked(useApi.bffFetch).mockClear();
    });

    it("renders chip when enabled:true and unservableCount > 0", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.resolve({
            enabled: true,
            gaps: [{ plugin: "blueocean", version: "1.25.3", requiredBy: "profile-a" }],
            gapCount: 1,
            unservable: [{ plugin: "blueocean", version: "1.25.3", requiredBy: "profile-a", reason: "ArchiveDisabled" }],
            unservableCount: 1,
            conditions: [], phase: "Degraded", pluginCount: 1, storeBytes: 0, lastSyncTime: null, storageType: "", pullThroughEnabled: false,
          });
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.getByText(/1 plugin the Update Center cannot serve/)).toBeInTheDocument();
      });
      const link = screen.getByRole("link", { name: /1 plugin the Update Center/ });
      expect(link).toHaveAttribute("href", "/administration/update-center");
    });

    it("does not render chip when enabled:false", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.resolve({ enabled: false, conditions: [], gaps: [], unservable: [], gapCount: 0, unservableCount: 0, lastSyncTime: null, phase: "", pluginCount: 0, storeBytes: 0, storageType: "", pullThroughEnabled: false });
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.queryByText(/cannot serve/)).not.toBeInTheDocument();
      });
    });

    it("does not render chip when gaps exist but nothing is unservable", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.resolve({
            enabled: true,
            gaps: [{ plugin: "blueocean", version: "1.25.3", requiredBy: "profile-a" }],
            gapCount: 1, unservable: [], unservableCount: 0,
            conditions: [], phase: "Ready", pluginCount: 0, storeBytes: 0, lastSyncTime: null, storageType: "", pullThroughEnabled: true,
          });
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.queryByText(/cannot serve/)).not.toBeInTheDocument();
      });
    });

    it("does not render chip when enabled:true but unservableCount is 0", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.resolve({ enabled: true, gaps: [], unservable: [], gapCount: 0, unservableCount: 0, conditions: [], phase: "Ready", pluginCount: 0, storeBytes: 0, lastSyncTime: null, storageType: "", pullThroughEnabled: false });
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.queryByText(/cannot serve/)).not.toBeInTheDocument();
      });
    });

    it("does not render chip when fetch fails and no dashboard error", async () => {
      vi.mocked(useApi.bffFetch).mockImplementation((path: string) => {
        if (path === "/updatecenter") {
          return Promise.reject(new Error("network error"));
        }
        return Promise.resolve({ items: [] });
      });
      mockUseControllers.mockReturnValue({
        data: [], isLoading: false, error: null,
      });
      renderWithProviders(<Dashboard />);
      await waitFor(() => {
        expect(screen.queryByText(/missing from Update Center/)).not.toBeInTheDocument();
      });
      // No dashboard-wide error banner should appear from the UC fetch failure
      expect(screen.queryByText(/Failed to load brood data/)).not.toBeInTheDocument();
    });
  });
});

describe("Dashboard needs-attention metric", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  const ctrl = (name: string) => ({
    name,
    namespace: "default",
    cluster: "core",
    phase: "Connected",
    endpoint: `https://${name}.example.com`,
    miteConnected: false,
  });

  it("counts every controller carrying attention, not only Failed phase", () => {
    mockUseControllers.mockReturnValue({
      data: [
        { ...ctrl("a"), phase: "Provisioning", attention: { kind: "reconcileBlocked", message: "plugin kubernetes requested at A conflicts with profile lock B" } },
        { ...ctrl("b"), phase: "Provisioning", attention: { kind: "bootFailed", message: "exit 5" } },
        { ...ctrl("c"), phase: "Failed", attention: { kind: "failed" } },
        { ...ctrl("d"), phase: "Connected", miteConnected: true },
      ],
      isLoading: false,
      error: null,
    });
    renderWithProviders(<Dashboard />);
    const card = screen.getByText("Needs attention").closest("div")!.parentElement!;
    expect(within(card).getByText("3")).toBeInTheDocument();
    expect(within(card).getByText("1 blocked \u00b7 1 boot failed \u00b7 1 failed")).toBeInTheDocument();
  });
});

describe("Dashboard brood health rollup", () => {
  it("lists controllers that need attention first and caps the list at eight", () => {
    const attention = { kind: "bootFailed" as const, message: "exit 5" };
    const ctrls = [
      createControllerListItem({ name: "zeta-ok", phase: "Connected", miteConnected: true }),
      createControllerListItem({ name: "bad-b", phase: "Failed", miteConnected: false, attention }),
      createControllerListItem({ name: "bad-a", phase: "Failed", miteConnected: false, attention }),
      createControllerListItem({ name: "down", phase: "Running", miteConnected: false }),
      createControllerListItem({ name: "sleep", phase: "Hibernated", miteConnected: false }),
    ];
    for (let i = 0; i < 7; i++) {
      ctrls.push(createControllerListItem({ name: `ok-${i}`, phase: "Connected", miteConnected: true }));
    }
    mockUseControllers.mockReturnValue({ data: ctrls, isLoading: false, error: null });
    renderWithProviders(<Dashboard />);

    const rows = screen.getAllByTestId("health-row");
    expect(rows).toHaveLength(8);
    expect(within(rows[0]).getByRole("link", { name: /bad-a/ })).toBeInTheDocument();
    expect(within(rows[1]).getByRole("link", { name: /bad-b/ })).toBeInTheDocument();
    expect(within(rows[2]).getByRole("link", { name: /\/down$/ })).toBeInTheDocument();
    expect(screen.getByText(/and 4 more/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "View all controllers" })).toHaveAttribute("href", "/controllers");
  });

  it("reads asleep for a hibernated controller and stopped for a stopped one", () => {
    const ctrls = [
      createControllerListItem({ name: "sleepy", phase: "Hibernated", miteConnected: false, jenkinsHealth: "unreachable" }),
      createControllerListItem({ name: "halted", phase: "Stopped", miteConnected: false, jenkinsHealth: "unreachable" }),
    ];
    mockUseControllers.mockReturnValue({ data: ctrls, isLoading: false, error: null });
    renderWithProviders(<Dashboard />);

    const rows = screen.getAllByTestId("health-row");
    // Both rank below the live rows, so they sort by name: halted, then sleepy.
    expect(within(rows[0]).getByText("stopped")).toBeInTheDocument();
    expect(within(rows[1]).getByText("asleep")).toBeInTheDocument();
    expect(screen.queryByText("unreachable")).not.toBeInTheDocument();
  });

  it("keeps unreachable for a running controller that lost its mite", () => {
    const ctrls = [
      createControllerListItem({ name: "lost", phase: "Running", miteConnected: false, jenkinsHealth: "unreachable" }),
    ];
    mockUseControllers.mockReturnValue({ data: ctrls, isLoading: false, error: null });
    renderWithProviders(<Dashboard />);

    expect(screen.getByText("unreachable")).toBeInTheDocument();
  });

  it("lists live controllers before hibernated ones", () => {
    const ctrls = [
      createControllerListItem({ name: "aaa-sleep", phase: "Hibernated", miteConnected: false }),
      createControllerListItem({ name: "zzz-live", phase: "Connected", miteConnected: true }),
    ];
    mockUseControllers.mockReturnValue({ data: ctrls, isLoading: false, error: null });
    renderWithProviders(<Dashboard />);

    const rows = screen.getAllByTestId("health-row");
    expect(within(rows[0]).getByRole("link", { name: /zzz-live/ })).toBeInTheDocument();
    expect(within(rows[1]).getByRole("link", { name: /aaa-sleep/ })).toBeInTheDocument();
  });

  it("renders a phase distribution bar with per-phase counts", () => {
    const ctrls = [
      createControllerListItem({ name: "a", phase: "Connected", miteConnected: true }),
      createControllerListItem({ name: "b", phase: "Connected", miteConnected: true }),
      createControllerListItem({ name: "c", phase: "Provisioning", miteConnected: false }),
      createControllerListItem({ name: "d", phase: "Failed", miteConnected: false, attention: { kind: "bootFailed", message: "exit 5" } }),
    ];
    mockUseControllers.mockReturnValue({ data: ctrls, isLoading: false, error: null });
    const { container } = renderWithProviders(<Dashboard />);

    const bar = screen.getByRole("img", { name: /Phase distribution/ });
    expect(bar).toHaveAttribute("aria-label", "Phase distribution: 2 Connected, 1 Provisioning, 1 Failed");
    expect(bar.querySelectorAll("[data-phase]")).toHaveLength(3);
    expect(container.querySelectorAll("[data-testid='health-row']")).toHaveLength(4);
    expect(screen.queryByText(/more ·/)).not.toBeInTheDocument();
  });
});

describe("Dashboard fan-out failures", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
  });

  const cluster = (name: string, core: boolean) => ({
    name,
    healthy: true,
    core,
    lastHeartbeat: new Date().toISOString(),
    operatorVersion: "1.0.0",
    k8sVersion: "1.30.0",
    controllerCount: 3,
    connectedCount: 2,
    state: "active" as const,
  });

  it("names a cluster whose controller list did not arrive", () => {
    mockFanoutFailures.mockReturnValue([{ name: "member-b", ok: false, error: "cluster unreachable" }]);
    renderWithProviders(<Dashboard />);
    expect(
      screen.getByRole("status"),
    ).toBeInTheDocument();
  });

  it("marks the strip card of a cluster whose list failed as list unavailable", () => {
    mockUseClusters.mockReturnValue({
      data: [cluster("core", true), cluster("member-b", false)],
      isLoading: false,
      error: null,
    } as never);
    mockFanoutFailures.mockReturnValue([{ name: "member-b", ok: false, error: "cluster unreachable" }]);
    renderWithProviders(<Dashboard />);
    expect(screen.getByText("list unavailable")).toBeInTheDocument();
    expect(screen.getByText("2/3 controllers")).toBeInTheDocument();
  });
});
