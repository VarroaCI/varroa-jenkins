import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import ControllerDetail, { PluginDriftPanel } from "./ControllerDetail";
import type { ActivityEvent } from "../types";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false } },
});

function renderWithProviders(ui: React.ReactElement) {
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>
  );
}

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ cluster: "core", namespace: "my-ns", name: "my-ctrl" }),
  };
});

vi.mock("../hooks/useClusters", () => ({
  useClusters: () => ({ data: [{ name: "core", core: true }], isLoading: false, isError: false }),
  coreOf: (c: unknown[]) => (c as any[])?.find((c2: any) => c2.core),
  clusterQuery: (c: string) => (c && c !== "core" ? `?cluster=${c}` : ""),
}));

vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    updateController: vi.fn(),
    ControllerConflictError: actual.ControllerConflictError,
    BFF_BASE: "/api/v1",
    approveRestart: vi.fn(),
    reprovisionController: vi.fn(),
    restartController: vi.fn(),
    setPowerState: vi.fn(),
    hibernateController: vi.fn(),
    wakeController: vi.fn(),
    getProvisioningConfig: vi.fn(() => Promise.resolve({ versions: [] })),
    getVersionProfiles: vi.fn(() => Promise.resolve([])),
    parsePreflightChecks: vi.fn(),
  };
});

const mockControllerData = {
  name: "my-ctrl",
  namespace: "my-ns",
  cluster: "core",
  phase: "Connected",
  endpoint: "https://builds.example.com",
  version: "2.462.1",
  jenkinsVersion: "2.462.1",
  jenkinsHealth: "healthy",
  miteConnected: true,
  miteVersion: "0.1.0",
  lastSeen: "2026-07-17T14:24:00Z",
  certExpiry: "2026-08-07T00:00:00Z",
  composedBundleRef: null as { name: string } | null,
  powerState: undefined as string | undefined,
  reconciliationPolicy: undefined as Record<string, string> | undefined,
  observability: undefined as any,
  lastApplyResult: undefined as any,
  appliedBundleHash: undefined as any,
  desiredStateHash: undefined as any,
  lastReconciledAt: undefined as any,
  liveDrift: undefined as any,
  pendingRestart: undefined as any,
  pendingItemDeletions: undefined as any,
  configHash: undefined as any,
  rbacHash: undefined as any,
  routingMode: "subdomain",
  miteImageStatus: undefined as any,
  rollout: undefined as any,
  versionStatus: undefined as any,
  probes: undefined as any,
  hibernation: undefined as any,
  resourceOverlay: undefined as any,
  podOverrides: undefined as any,
  conditions: undefined as any,
  applyHistory: undefined as any,
  pluginInventory: undefined as any,
};

const mockBffFetch = vi.fn((_path?: unknown, _options?: unknown) => Promise.resolve({}));
vi.mock("../hooks/useApi", async () => {
  const actual = await vi.importActual<typeof import("../hooks/useApi")>("../hooks/useApi");
  return {
    ...actual,
    bffFetch: (path: unknown, options?: unknown) => mockBffFetch(path, options),
  };
});

const mockUseController = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useController: () => mockUseController(),
}));

const mockUseComposedBundles = vi.fn();
vi.mock("../hooks/useCatalog", () => ({
  useComposedBundles: () => mockUseComposedBundles(),
}));

vi.mock("../hooks/useEventStream", () => {
  const mock = vi.fn(() => ({ lastEvent: null, readyState: "closed", error: null }));
  return { useEventStream: mock, __esModule: true };
});

const mockUseActivityFeed = vi.fn(() => ({
  events: [] as ActivityEvent[],
  pendingCount: 0,
  paused: false,
  setPaused: vi.fn(),
  resume: vi.fn(),
  readyState: "closed",
  error: null,
  isLoading: false,
  hasMore: false,
  loadMore: vi.fn(),
  isLoadingMore: false,
}));
vi.mock("../hooks/useActivityFeed", () => ({
  useActivityFeed: () => mockUseActivityFeed(),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
}));

const mockCanDo = vi.fn((_perms?: unknown, _ns?: string, _resource?: string, _verb?: string): boolean => true);
vi.mock("../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: { global: {}, scopes: [] } }),
  canDoInNamespace: (perms: unknown, ns: string, resource: string, verb: string) => mockCanDo(perms, ns, resource, verb),
}));

vi.mock("../components/Toast", () => ({
  useToast: () => ({ toast: vi.fn() }),
}));

vi.mock("../components/BundleSelector", () => ({
  BundleSelector: () => <div data-testid="bundle-selector" />,
  BundleHealthBadge: ({ phase }: { phase?: string }) => <span data-testid={`badge-${phase ?? "none"}`}>[{phase ?? "no-phase"}]</span>,
}));

function setCtrl(overrides: Record<string, any>) {
  mockUseController.mockReturnValue({
    data: { ...mockControllerData, ...overrides },
    isLoading: false,
    error: null,
  });
}

function setBundles(items: unknown[]) {
  mockUseComposedBundles.mockReturnValue({
    data: { items },
    isLoading: false,
    error: null,
  });
}

function setFeed(events: ActivityEvent[], readyState = "open") {
  mockUseActivityFeed.mockReturnValue({
    events,
    pendingCount: 0,
    paused: false,
    setPaused: vi.fn(),
    resume: vi.fn(),
    readyState,
    error: null,
    isLoading: false,
    hasMore: false,
    loadMore: vi.fn(),
    isLoadingMore: false,
  });
}

const phaseEvent: ActivityEvent = {
  timestamp: "2026-07-17T14:09:00Z",
  type: "phase",
  source: "mite",
  namespace: "my-ns",
  controller: "my-ctrl",
  cluster: "core",
  phase: "Connected",
  message: "phase changed from Provisioning to Connected",
};

const itemEvent: ActivityEvent = {
  timestamp: "2026-07-17T14:08:00Z",
  type: "job.created",
  source: "operator",
  namespace: "my-ns",
  controller: "my-ctrl",
  cluster: "core",
  message: "Created acceptance-folder/hello-pipeline",
};

describe("ControllerDetail — Overview activity feed (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    setFeed([phaseEvent, itemEvent]);
    mockCanDo.mockImplementation(() => true);
  });

  it("renders the most recent activity entries with a Full history link", () => {
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/phase changed from Provisioning to Connected/)).toBeInTheDocument();
    expect(screen.getByText(/Created acceptance-folder\/hello-pipeline/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /full history/i })).toBeInTheDocument();
  });

  it("presents the overview activity feed as a labelled section", () => {
    renderWithProviders(<ControllerDetail />);
    // The feed is a distinct card with a real heading, not an unlabelled run of
    // rows on the page background. The Full history control lives in its header.
    const heading = screen.getByRole("heading", { name: "Activity" });
    expect(heading).toBeInTheDocument();
    const header = heading.parentElement!;
    expect(within(header).getByRole("button", { name: /full history/i })).toBeInTheDocument();
  });

  it("caps the feed at five entries", () => {
    setFeed(Array.from({ length: 8 }, (_, i) => ({
      timestamp: `2026-07-17T14:${String(i).padStart(2, "0")}:00Z`,
      type: "phase",
      source: "mite",
      namespace: "my-ns",
      controller: "my-ctrl",
      cluster: "core",
      message: `event ${i}`,
    })));
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText(/event \d/).length).toBe(5);
  });

  it("Full history switches to the Diagnostics tab", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /full history/i }));
    expect(screen.getByTestId("diagnostics-tab")).toBeInTheDocument();
  });
});

describe("ControllerDetail — header phase pill (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    setFeed([phaseEvent]);
    mockCanDo.mockImplementation(() => true);
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-17T14:25:00Z"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows the phase pill with a relative statechange from the newest phase event", () => {
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(screen.getByText("for 16m")).toBeInTheDocument();
  });

  it("shows an Applying pill while an apply is in flight", () => {
    setCtrl({ desiredStateHash: "new", lastApplyResult: { hash: "old", succeeded: true, sections: [], timestamp: "2026-07-17T14:00:00Z" } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Applying")).toBeInTheDocument();
  });

  it("shows an Awaiting approval pill for a manual-mode hash mismatch instead of Applying", () => {
    setCtrl({
      reconciliationPolicy: { mode: "manual", interval: "30s" },
      desiredStateHash: "new",
      lastApplyResult: { hash: "old", succeeded: true, sections: [], timestamp: "2026-07-17T14:00:00Z" },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Awaiting approval")).toBeInTheDocument();
    expect(screen.queryByText("Applying")).not.toBeInTheDocument();
  });

  it("shows an Apply failed pill for a manual-mode hash mismatch when the apply actually failed", () => {
    setCtrl({
      reconciliationPolicy: { mode: "manual", interval: "30s" },
      desiredStateHash: "new",
      lastApplyResult: { hash: "old", succeeded: false, sections: [{ name: "rbac", ok: false }], timestamp: "2026-07-17T14:00:00Z" },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Apply failed")).toBeInTheDocument();
    expect(screen.queryByText("Awaiting approval")).not.toBeInTheDocument();
  });

  it("shows an Apply failed pill after a rejected apply", () => {
    setCtrl({ lastApplyResult: { succeeded: false, hash: "old", sections: [{ name: "rbac", ok: false }], timestamp: "2026-07-17T14:00:00Z" } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Apply failed")).toBeInTheDocument();
  });

  it("shows Boot failed on the phase pill and a banner while Provisioning", () => {
    setCtrl({
      phase: "Provisioning",
      attention: {
        kind: "bootFailed",
        reason: "JenkinsBootFailed",
        message: "jenkins container exited with code 5 (283 restarts): Error",
        since: "2026-09-01T10:00:00Z",
      },
    });
    renderWithProviders(<ControllerDetail />);
    // "Boot failed" appears twice: on the phase pill and as the banner title.
    // This describe block runs on fake timers, so queries must be synchronous.
    expect(screen.getAllByText("Boot failed")).toHaveLength(2);
    expect(screen.getByText(/exited with code 5 \(283 restarts\)/)).toBeInTheDocument();
    expect(screen.getByText(/Jenkins container logs/)).toBeInTheDocument();
  });
});

describe("ControllerDetail — quiet hibernation (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    mockCanDo.mockImplementation(() => true);
  });

  it("hibernated: quiet card, Wake button, no Open Jenkins, no pipeline", () => {
    setCtrl({
      hibernated: true,
      phase: "Hibernated",
      appliedBundleHash: "31ae2452abcdef",
      observability: { summary: { totalJobs: 4 } },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Sleeping — scaled to zero")).toBeInTheDocument();
    expect(screen.getByText(/Wakes on the first inbound request/)).toBeInTheDocument();
    expect(screen.getByText(/config preserved at/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Wake$/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Open Jenkins/ })).not.toBeInTheDocument();
    expect(screen.queryByTestId("delivery-pipeline")).not.toBeInTheDocument();
    expect(screen.queryByText(/metrics unavailable/)).not.toBeInTheDocument();
  });

  it("stopped: Powered off heading and Power On action", () => {
    setCtrl({ powerState: "Stopped", phase: "Stopped" });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Powered off")).toBeInTheDocument();
    expect(screen.getByText("Stopped — not serving. Power on to resume.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Power On/ })).toBeInTheDocument();
  });

  it("reconcile button becomes Reconciling… and disables while the request is in flight", async () => {
    const user = userEvent.setup();
    let resolveReconcile: (v: {}) => void = () => {};
    mockBffFetch.mockImplementation((path: unknown) => {
      if (String(path).includes("/reconcile")) {
        return new Promise<{}>((resolve) => { resolveReconcile = resolve; });
      }
      return Promise.resolve({});
    });
    setCtrl({});
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /^⟳ Reconcile$/i }));
    const pending = screen.getByRole("button", { name: /Reconciling…/i });
    expect(pending).toBeDisabled();
    resolveReconcile({});
    await waitFor(() => expect(screen.getByRole("button", { name: /^⟳ Reconcile$/i })).toBeEnabled());
  });
});

describe("ControllerDetail — Diagnostics panels (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    setFeed([]);
    mockCanDo.mockImplementation(() => true);
  });

  it("renders Connection and Conditions panels with all-clear", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    expect(screen.getByText("Connection")).toBeInTheDocument();
    expect(screen.getByText("Conditions")).toBeInTheDocument();
    expect(screen.getByText(/grpc mTLS · stream open/)).toBeInTheDocument();
    expect(screen.getByText("0.1.0")).toBeInTheDocument();
    expect(screen.getByText("All clear — nothing needs attention")).toBeInTheDocument();
  });

  it("lists conditions when present", async () => {
    const user = userEvent.setup();
    setCtrl({ conditions: [{ type: "HibernationCronTriggersSkipped", status: "True", message: "cron paused" }] });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    expect(screen.getByText("HibernationCronTriggersSkipped")).toBeInTheDocument();
    expect(screen.getByText("cron paused")).toBeInTheDocument();
    expect(screen.queryByText(/All clear/)).not.toBeInTheDocument();
  });

  it("renders Apply History rows with section chips", async () => {
    const user = userEvent.setup();
    setCtrl({
      applyHistory: [
        { hash: "abc123def456", timestamp: "2026-07-17T14:00:00Z", succeeded: true, sections: [{ name: "config", ok: true }, { name: "rbac", ok: true }], trigger: "reconciliation" },
        { hash: "deadbeefcafe", timestamp: "2026-07-16T10:00:00Z", succeeded: false, sections: [{ name: "plugins", ok: false }], trigger: "bundle change" },
      ],
    });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    expect(screen.getByText(/Apply History/)).toBeInTheDocument();
    expect(screen.getByText("abc123def456")).toBeInTheDocument();
    expect(screen.getByText("deadbeefcafe")).toBeInTheDocument();
    expect(screen.getByText("bundle change")).toBeInTheDocument();
    expect(screen.getByText(/✗ plugins/)).toBeInTheDocument();
  });

  it("labels the Connection certificate 'expires' with the correct local calendar day", async () => {
    // certExpiry is a date-only string; the dd must render the expiry in the
    // viewer's local timezone (no UTC-midnight one-day shift) and say
    // "expires", not "renews".
    const user = userEvent.setup();
    setCtrl({ certExpiry: "2026-08-20" });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    expect(screen.getByText(`expires ${new Date(2026, 7, 20).toLocaleDateString()}`)).toBeInTheDocument();
    expect(screen.queryByText(/renews/)).not.toBeInTheDocument();
  });

  it("caps the merged stream at 500 entries (documented browser buffer), not 400", async () => {
    // 520 activity events flow into ControllerStream; the merged, sorted
    // entries list must be truncated to 500 (docs/operations/observability.md),
    // matching the 500-line cap applied to log lines in the same component.
    setFeed(
      Array.from({ length: 520 }, (_, i) => ({
        timestamp: `2026-07-17T14:${String(Math.floor(i / 60)).padStart(2, "0")}:${String(i % 60).padStart(2, "0")}Z`,
        type: "info",
        source: "operator",
        namespace: "my-ns",
        controller: "my-ctrl",
        cluster: "core",
        message: `stream event ${i}`,
      })),
    );
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    expect(screen.getAllByText(/stream event \d+/)).toHaveLength(500);
  });
});

describe("PluginDriftPanel", () => {
  const ctrl = (pi: any) => ({ pluginInventory: pi }) as any;

  it("shows empty state when pluginInventory is absent", () => {
    render(<PluginDriftPanel ctrl={ctrl(undefined)} />);
    expect(screen.getByText(/No plugin inventory available yet/)).toBeInTheDocument();
  });

  it("renders counts from the inventory", () => {
    const { container } = render(
      <PluginDriftPanel
        ctrl={ctrl({
          hash: "v1:test",
          source: "jenkins-api",
          stale: false,
          degraded: false,
          bootstrapApproximate: false,
          optionalEdgesDropped: false,
          truncated: false,
          total: 76,
          driftTruncated: false,
          counts: { declared: 76, unmanaged: 5 },
          drift: [{ name: "rogue", version: "1.0", class: "unmanaged" }],
          versionDrift: [{ name: "git", version: "4.11.4", verdict: "ahead" }],
        })}
      />,
    );
    const counts = container.querySelector('[class*="counts"]');
    expect(counts!.textContent).toContain("76");
    expect(counts!.textContent).toContain("declared");
    expect(counts!.textContent).not.toContain("installed");
    expect(counts!.textContent).toContain("unmanaged");
    expect(counts!.textContent).toContain("1 version drift");
    expect(screen.getByText(/unmanaged — not in the bundle/)).toBeInTheDocument();
    expect(screen.getByText(/ahead — pin differs/)).toBeInTheDocument();
  });

  it("counts every versionDrift entry in the version-drift figure", () => {
    const { container } = render(
      <PluginDriftPanel
        ctrl={ctrl({
          hash: "h",
          source: "s",
          total: 5,
          stale: false,
          degraded: false,
          bootstrapApproximate: false,
          optionalEdgesDropped: false,
          truncated: false,
          driftTruncated: false,
          versionDrift: [
            { name: "git", version: "1.0", verdict: "ahead" },
            { name: "git-client", version: "2.0", verdict: "behind" },
            { name: "missing-plugin", version: "3.0", verdict: "missing" },
            { name: "another-ahead", version: "4.0", verdict: "ahead" },
          ],
        })}
      />,
    );
    const counts = container.querySelector('[class*="counts"]');
    expect(counts!.textContent).toContain("4 version drift");
    expect(counts!.textContent).not.toContain("2 version drift");
  });

  it("labels optional-dependency drift with the transitive text, not 'unmanaged'", () => {
    render(
      <PluginDriftPanel
        ctrl={ctrl({
          hash: "h",
          source: "s",
          total: 5,
          stale: false,
          degraded: false,
          bootstrapApproximate: false,
          optionalEdgesDropped: false,
          truncated: false,
          driftTruncated: false,
          drift: [{ name: "git-client", version: "1.2", class: "optional-dependency" }],
        })}
      />,
    );
    expect(screen.getByText(/optional dependency — pulled in transitively, not declared/)).toBeInTheDocument();
    expect(screen.queryByText(/unmanaged — not in the bundle/)).not.toBeInTheDocument();
  });

  it("renders '—' for declared/unmanaged when counts is entirely absent", () => {
    const { container } = render(
      <PluginDriftPanel
        ctrl={ctrl({
          hash: "h",
          source: "s",
          total: 76,
          stale: false,
          degraded: false,
          bootstrapApproximate: false,
          optionalEdgesDropped: false,
          truncated: false,
          driftTruncated: false,
        })}
      />,
    );
    const counts = container.querySelector('[class*="counts"]');
    // Neither declared nor unmanaged should show a false "0" when counts is absent.
    expect(counts!.textContent).toContain("— declared");
    expect(counts!.textContent).toContain("— unmanaged");
    expect(counts!.textContent).not.toContain("0 declared");
    expect(counts!.textContent).not.toContain("0 unmanaged");
    // No fake "installed" figure (there is no such counts key); the drift
    // count (from versionDrift) still renders a real value even when it is zero.
    expect(counts!.textContent).not.toContain("installed");
    expect(counts!.textContent).toContain("0 version drift");
  });

  it("renders '—' for declared/unmanaged when counts is present but its keys are missing", () => {
    const { container } = render(
      <PluginDriftPanel
        ctrl={ctrl({
          hash: "h",
          source: "s",
          total: 76,
          stale: false,
          degraded: false,
          bootstrapApproximate: false,
          optionalEdgesDropped: false,
          truncated: false,
          driftTruncated: false,
          // counts is present as an object, but neither declared nor unmanaged
          // is reported inside it — a loosely-typed map missing a key means
          // "not reported", the same ambiguity as counts entirely absent.
          counts: {},
        })}
      />,
    );
    const counts = container.querySelector('[class*="counts"]');
    // Both must render "—", not a false "0", when the keys are missing.
    expect(counts!.textContent).toContain("— declared");
    expect(counts!.textContent).toContain("— unmanaged");
    expect(counts!.textContent).not.toContain("0 declared");
    expect(counts!.textContent).not.toContain("0 unmanaged");
    // No fake "installed" figure even though total is present.
    expect(counts!.textContent).not.toContain("installed");
  });

  it("caps drift rows at three and expands with Show all", () => {
    const drift = Array.from({ length: 5 }, (_, i) => ({ name: `plugin-${i}`, version: "1.0", class: "unmanaged" }));
    render(<PluginDriftPanel ctrl={ctrl({ hash: "h", source: "s", total: 5, stale: false, degraded: false, bootstrapApproximate: false, optionalEdgesDropped: false, truncated: false, driftTruncated: false, drift })} />);
    expect(screen.getByText("Show all 5 →")).toBeInTheDocument();
    expect(screen.getByText(/plugin-0/)).toBeInTheDocument();
    expect(screen.queryByText(/plugin-4/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByText("Show all 5 →"));
    expect(screen.getByText(/plugin-4/)).toBeInTheDocument();
  });

  it("shows bootstrap and jenkins-supplied counts only in the expanded view, never a fake 'installed' figure", () => {
    const { container } = render(
      <PluginDriftPanel
        ctrl={ctrl({
          hash: "h",
          source: "s",
          total: 76,
          stale: false,
          degraded: false,
          bootstrapApproximate: false,
          optionalEdgesDropped: false,
          truncated: false,
          driftTruncated: false,
          counts: { bootstrap: 2, declared: 76, "jenkins-supplied": 3, unmanaged: 5 },
          drift: Array.from({ length: 4 }, (_, i) => ({ name: `rogue-${i}`, version: "1.0", class: "unmanaged" })),
        })}
      />,
    );
    const headline = container.querySelector('[class*="counts"]');
    // Headline line: declared · unmanaged · version drift — no "installed".
    expect(headline!.textContent).toContain("76 declared");
    expect(headline!.textContent).toContain("5 unmanaged");
    expect(headline!.textContent).toContain("0 version drift");
    expect(headline!.textContent).not.toContain("installed");
    // Secondary breakdowns stay hidden until the view is expanded.
    expect(container.querySelectorAll('[class*="counts"]').length).toBe(1);
    expect(screen.queryByText(/bootstrap/)).not.toBeInTheDocument();
    expect(screen.queryByText(/jenkins-supplied/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByText(/Show all/));
    const counts = container.querySelectorAll('[class*="counts"]');
    expect(counts.length).toBe(2);
    expect(counts[1].textContent).toContain("2 bootstrap");
    expect(counts[1].textContent).toContain("3 jenkins-supplied");
  });

  it("shows a details toggle when there are few drift rows but bootstrap/jenkins-supplied counts exist", () => {
    const { container } = render(
      <PluginDriftPanel
        ctrl={ctrl({
          hash: "h",
          source: "s",
          total: 76,
          stale: false,
          degraded: false,
          bootstrapApproximate: false,
          optionalEdgesDropped: false,
          truncated: false,
          driftTruncated: false,
          counts: { bootstrap: 2, declared: 76, "jenkins-supplied": 3, unmanaged: 5 },
          drift: Array.from({ length: 2 }, (_, i) => ({ name: `rogue-${i}`, version: "1.0", class: "unmanaged" })),
        })}
      />,
    );
    // Fewer than four rows, but secondary counts exist — the toggle must still
    // appear ("Show details", no row count) so those counts are reachable.
    expect(screen.getByText("Show details")).toBeInTheDocument();
    expect(screen.queryByText(/bootstrap/)).not.toBeInTheDocument();
    expect(screen.queryByText(/jenkins-supplied/)).not.toBeInTheDocument();
    expect(container.querySelectorAll('[class*="counts"]').length).toBe(1);

    fireEvent.click(screen.getByText("Show details"));
    expect(screen.getByText("Show fewer")).toBeInTheDocument();
    const counts = container.querySelectorAll('[class*="counts"]');
    expect(counts.length).toBe(2);
    expect(counts[1].textContent).toContain("2 bootstrap");
    expect(counts[1].textContent).toContain("3 jenkins-supplied");
  });

  it("shows a stale qualifier badge when the inventory is stale", () => {
    render(
      <PluginDriftPanel
        ctrl={ctrl({ hash: "h", source: "s", total: 5, stale: true, degraded: false, bootstrapApproximate: false, optionalEdgesDropped: false, truncated: false, driftTruncated: false })}
      />,
    );
    expect(screen.getByText("stale — last synced data")).toBeInTheDocument();
  });

  it("shows a degraded qualifier badge when the inventory is degraded", () => {
    render(
      <PluginDriftPanel
        ctrl={ctrl({ hash: "h", source: "s", total: 5, stale: false, degraded: true, bootstrapApproximate: false, optionalEdgesDropped: false, truncated: false, driftTruncated: false })}
      />,
    );
    expect(screen.getByText("degraded")).toBeInTheDocument();
  });

  it("qualifies the empty-drift state when the inventory is truncated", () => {
    render(
      <PluginDriftPanel
        ctrl={ctrl({ hash: "h", source: "s", total: 5, stale: false, degraded: false, bootstrapApproximate: false, optionalEdgesDropped: false, truncated: true, driftTruncated: false })}
      />,
    );
    expect(screen.getByText(/not a confirmed clean drift check/)).toBeInTheDocument();
    expect(screen.queryByText(/everything matches the bundle/)).not.toBeInTheDocument();
  });

  it("names optional-edge drops specifically when only that flag is set", () => {
    render(
      <PluginDriftPanel
        ctrl={ctrl({ hash: "h", source: "s", total: 5, stale: false, degraded: false, bootstrapApproximate: false, optionalEdgesDropped: true, truncated: false, driftTruncated: false })}
      />,
    );
    expect(screen.getByText(/optional dependency edges were dropped from the collected graph/)).toBeInTheDocument();
    expect(screen.getByText(/not a confirmed clean drift check/)).toBeInTheDocument();
    // A dropped optional edge is not a hard truncation — the reason string
    // must say what actually happened, not fall back to "(truncated)".
    expect(screen.queryByText(/truncated/)).not.toBeInTheDocument();
  });
});
