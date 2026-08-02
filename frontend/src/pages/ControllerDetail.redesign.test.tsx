import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import ControllerDetail from "./ControllerDetail";

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

let mockRouteCluster = "core";
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ cluster: mockRouteCluster, namespace: "my-ns", name: "my-ctrl" }),
  };
});
afterEach(() => { mockRouteCluster = "core"; });

vi.mock("../hooks/useClusters", () => ({
  useClusters: () => ({ data: [{name:"core",core:true}], isLoading: false, isError: false }),
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
  miteConnected: true,
  composedBundleRef: null as { name: string } | null,
  powerState: undefined as string | undefined,
  reconciliationPolicy: undefined as Record<string, string> | undefined,
  observability: undefined as any,
  lastApplyResult: undefined as any,
  lastReconciledAt: undefined as string | undefined,
  liveDrift: undefined as any,
  lastSeen: undefined as string | undefined,
  pendingRestart: undefined as any,
  pendingItemDeletions: undefined as any,
  configHash: undefined as any,
  rbacHash: undefined as string | undefined,
  desiredStateHash: undefined as string | undefined,
  appliedBundleHash: undefined as string | undefined,
  routingMode: "subdomain",
  jenkinsHealth: "Unknown",
  miteVersion: undefined as string | undefined,
  certExpiry: undefined as string | undefined,
  rollout: undefined as any,
  versionStatus: undefined as any,
  probes: undefined as any,
  hibernation: undefined as any,
  resourceOverlay: undefined as any,
  podOverrides: undefined as any,
  conditions: undefined as any,
  applyHistory: undefined as any,
};

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
  BundleSelector: ({ value, onChange }: { namespace: string; value: string | null; onChange: (name: string | null) => void }) => (
    <div data-testid="bundle-selector">
      <span data-testid="bundle-value">{value ?? "(none)"}</span>
      <button data-testid="select-bundle-x" onClick={() => onChange("bundle-x")}>Select Bundle X</button>
    </div>
  ),
  BundleHealthBadge: ({ phase }: { phase?: string }) => <span data-testid={`badge-${phase ?? "none"}`}>[{phase ?? "no-phase"}]</span>,
}));

import { useEventStream } from "../hooks/useEventStream";

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

/* ====================================================================== */
/*  8.2 — Verdict precedence                                              */
/* ====================================================================== */

describe("ControllerDetail — Verdict precedence (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("disconnected mite outranks everything — no failed apply verdict", () => {
    setCtrl({
      miteConnected: false,
      lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }] },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Mite disconnected/)).toBeInTheDocument();
    expect(screen.queryByText(/Last apply failed/)).not.toBeInTheDocument();
  });

  it("failed apply renders section names in verdict head", () => {
    setCtrl({
      lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }] },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/the rbac section was rejected/)).toBeInTheDocument();
  });

  it("bundle-invalid shows Fix bundle and Detach when canUpdate", () => {
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Invalid", errors: ["bad"] } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Bundle is invalid/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /fix bundle/i })).toBeInTheDocument();
    const detachBtns = screen.getAllByRole("button", { name: /detach/i });
    expect(detachBtns.length).toBeGreaterThanOrEqual(1);
  });

  it("bundle-invalid hides Detach when !canUpdate", () => {
    mockCanDo.mockImplementation((_p?: any, _ns?: string, r?: string, v?: string) => !(r === "controllers" && v === "update"));
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Invalid", errors: ["bad"] } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Bundle is invalid/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /detach/i })).not.toBeInTheDocument();
  });

  it("drift verdict shows exact copy", () => {
    setCtrl({ liveDrift: { detected: true, liveConfigHash: "abc" } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Configuration drift detected/)).toBeInTheDocument();
    expect(screen.getByText(/Reconcile to converge/)).toBeInTheDocument();
  });

  it("healthy verdict shows converged message", () => {
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Healthy — desired state converged/)).toBeInTheDocument();
  });
});

/* ====================================================================== */
/*  Confirm gating — exact body copy + blast-radius                       */
/* ====================================================================== */

describe("ControllerDetail — Confirm copy (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({ powerState: "Running" });
    mockCanDo.mockImplementation(() => true);
  });

  it("restart confirm shows exact body and zero-builds string", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^restart rolls/i }));
    expect(screen.getByText(/Rolls the Jenkins pod/)).toBeInTheDocument();
    expect(screen.getByText(/No running builds will be interrupted/)).toBeInTheDocument();
  });

  it("reprovision confirm shows exact body", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /reprovision/i }));
    expect(screen.getByText(/Destroys and rebuilds the StatefulSet/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /yes, reprovision/i })).toBeInTheDocument();
  });

  it("power-off confirm shows exact body", async () => {
    const user = userEvent.setup();
    setCtrl({ powerState: "Running" });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^power off stops/i }));
    expect(screen.getByText(/Stops the controller/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /yes, power off/i })).toBeInTheDocument();
  });

  it("hibernate confirm shows exact body", async () => {
    const user = userEvent.setup();
    setCtrl({ powerState: "Running" });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /hibernate/i }));
    expect(screen.getByText(/Scales the controller to zero/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /yes, hibernate/i })).toBeInTheDocument();
  });

  it("singular blast-radius with runningBuilds=1", () => {
    setCtrl({
      lastApplyResult: { succeeded: false, sections: [{ name: "x", ok: false }] },
      observability: { summary: { runningBuilds: 1 } },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText(/1 running build/).length).toBeGreaterThanOrEqual(1);
  });

  it("plural blast-radius with runningBuilds=3", () => {
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "x", ok: false }] }, observability: { summary: { runningBuilds: 3 } } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText(/3 running builds/).length).toBeGreaterThanOrEqual(1);
  });

  it("zero builds string when no running builds", async () => {
    const user = userEvent.setup();
    setCtrl({
      powerState: "Running",
      lastApplyResult: { succeeded: false, sections: [{ name: "x", ok: false, error: "err" }] },
      observability: { summary: { runningBuilds: 0 } },
    });
    renderWithProviders(<ControllerDetail />);
    // Open overflow menu and click Restart to trigger confirm dialog
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^restart rolls/i }));
    expect(screen.getByText(/No running builds will be interrupted/)).toBeInTheDocument();
  });
});

/* ====================================================================== */
/*  Menu a11y: Escape closes + focus returns                              */
/* ====================================================================== */

describe("ControllerDetail — Menu a11y (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("Escape closes the overflow menu and returns focus to the trigger", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    const trigger = screen.getByRole("button", { name: /more actions/i });
    await user.click(trigger);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });
});

/* ====================================================================== */
/*  Tile sub copy                                                         */
/* ====================================================================== */

describe("ControllerDetail — Tile sub copy (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    mockCanDo.mockImplementation(() => true);
  });

  it("shows 'matches desired' when versions match", () => {
    setCtrl({ jenkinsVersion: "2.462.1", version: "2.462.1" });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("matches desired")).toBeInTheDocument();
  });

  it("shows drift tile sub when versions differ", () => {
    setCtrl({ jenkinsVersion: "2.461.0", version: "2.462.1" });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/desired 2.462.1/)).toBeInTheDocument();
  });

  it("shows 'reported by mite' when mite connected", () => {
    setCtrl({ miteConnected: true, observability: { summary: { totalJobs: 42 } } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("reported by mite")).toBeInTheDocument();
  });

  it("shows 'unknown — mite offline' when mite disconnected", () => {
    setCtrl({ miteConnected: false });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText(/unknown — mite offline/).length).toBeGreaterThanOrEqual(1);
  });

  it("shows 'idle' when mite connected and 0 running builds", () => {
    setCtrl({ miteConnected: true, observability: { summary: { runningBuilds: 0 } } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("idle")).toBeInTheDocument();
  });

  it("shows 'a restart would terminate these' when builds > 0", () => {
    setCtrl({ miteConnected: true, observability: { summary: { runningBuilds: 2 } } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("a restart would terminate these")).toBeInTheDocument();
  });

  it("shows last reconciled tile sub with interval and mode", () => {
    setCtrl({ lastReconciledAt: "2026-01-01T00:00:00Z", reconciliationPolicy: { interval: "30s", mode: "automatic" } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText(/every 30s/).length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText(/· automatic/).length).toBeGreaterThanOrEqual(1);
  });
});

/* ====================================================================== */
/*  Overview composition                                                  */
/* ====================================================================== */

describe("ControllerDetail — Overview composition (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("tiles render before pipeline and grid", () => {
    renderWithProviders(<ControllerDetail />);
    // Tiles (Jenkins version, Jobs, Running builds, Last reconciled)
    expect(screen.getByText("Jenkins version")).toBeInTheDocument();
    expect(screen.getByText("Jobs")).toBeInTheDocument();
    expect(screen.getByText("Running builds")).toBeInTheDocument();
    expect(screen.getByText("Last reconciled")).toBeInTheDocument();
  });

  it("BundleCard renders on Overview when canUpdate", () => {
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Ready" } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("bundle-x")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Change" })).toBeInTheDocument();
  });

  it("BundleCard read-only without canUpdate (no Change/Detach)", () => {
    mockCanDo.mockImplementation((_p?: any, _ns?: string, r?: string, v?: string) => !(r === "controllers" && v === "update"));
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Ready" } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("bundle-x")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Change" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Detach" })).not.toBeInTheDocument();
  });
});

/* ====================================================================== */
/*  Wake / Power On firing immediately                                    */
/* ====================================================================== */

describe("ControllerDetail — Wake/Power On (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    mockCanDo.mockImplementation(() => true);
  });

  it("Wake fires immediately when hibernated (no confirm)", async () => {
    const { setPowerState } = await import("../api/client");
    const user = userEvent.setup();
    setCtrl({ powerState: "Hibernated", phase: "Hibernated" });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /Wake/ }));
    expect(setPowerState).toHaveBeenCalled();
  });

  it("Power On fires immediately when stopped (no confirm)", async () => {
    const { setPowerState } = await import("../api/client");
    const user = userEvent.setup();
    setCtrl({ powerState: "Stopped", phase: "Stopped" });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /Power On/ }));
    expect(setPowerState).toHaveBeenCalled();
  });
});

/* ====================================================================== */
/*  Not ready / View embedded                                             */
/* ====================================================================== */

describe("ControllerDetail — Not ready and View embedded (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("shows 'Not ready' when endpoint absent", () => {
    setCtrl({ endpoint: undefined });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByRole("button", { name: /not ready/i })).toBeDisabled();
  });
});

/* ====================================================================== */
/*  Gate preservation                                                     */
/* ====================================================================== */

describe("ControllerDetail — Gate preservation (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
  });

  it("approve-restart only: sees pipeline Reload/Restart but no Reprovision or header menu", async () => {
    mockCanDo.mockImplementation((_p?: any, _ns?: string, r?: string, v?: string) => r !== "controllers" || v === "approve-restart");
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    // No header menu (needs manage)
    expect(screen.queryByRole("button", { name: /more actions/i })).not.toBeInTheDocument();
    // Open configuration to check pipeline buttons
    await user.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByTestId("configuration-tab")).toBeInTheDocument());
    // Pipeline surfaces gated by approve-restart render; Reprovision (manage) does not
    expect(screen.getByRole("button", { name: "Reload" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Restart" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /reprovision/i })).not.toBeInTheDocument();
  });

  it("read-only viewer (no permissions): BundleCard has no Change/Detach", () => {
    mockCanDo.mockImplementation(() => false);
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Ready" } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.queryByRole("button", { name: "Change" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Detach" })).not.toBeInTheDocument();
  });
});

/* ====================================================================== */
/*  Sections band                                                         */
/* ====================================================================== */

describe("ControllerDetail — Sections band (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("shows band count header", async () => {
    const user = userEvent.setup();
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }, { name: "config", ok: true }] } });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByText(/1\/2 applied/)).toBeInTheDocument());
  });

  it("shows error text from backend", async () => {
    const user = userEvent.setup();
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }] } });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByText(/denied/)).toBeInTheDocument());
  });

  it("shows fallback string when error absent", async () => {
    const user = userEvent.setup();
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false }] } });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByText(/rejected, no message returned/)).toBeInTheDocument());
  });
});

/* ====================================================================== */
/*  Hash disclosure                                                       */
/* ====================================================================== */

describe("ControllerDetail — Hash disclosure (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("shows 'In sync' chip when converged", async () => {
    const user = userEvent.setup();
    setCtrl({
      lastApplyResult: { succeeded: true, hash: "abc" },
      desiredStateHash: "abc",
      configHash: "abc",
    });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByText(/In sync/)).toBeInTheDocument());
  });

  it("shows 'Out of sync' chip when not converged", async () => {
    const user = userEvent.setup();
    setCtrl({
      lastApplyResult: { succeeded: true, hash: "abc" },
      desiredStateHash: "def",
      configHash: "abc",
    });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByText(/Out of sync/)).toBeInTheDocument());
  });

  it("shows hash count summary", async () => {
    setCtrl({
      lastApplyResult: { succeeded: true, hash: "abc" },
      desiredStateHash: "abc",
      configHash: "abc123456789",
    });
    renderWithProviders(<ControllerDetail />);
    fireEvent.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByTestId("configuration-tab")).toBeInTheDocument());
    // The disclosure shows "{N} hashes" in the summary
    const disclosure = screen.getByText(/hashes/);
    expect(disclosure).toBeInTheDocument();
  });

  it("copy button has aria-label with hash label", async () => {
    const user = userEvent.setup();
    setCtrl({
      lastApplyResult: { succeeded: true, hash: "abc" },
      desiredStateHash: "abc",
      configHash: "abc123456789",
    });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getAllByText(/Configuration/)[0]);
    await waitFor(() => expect(screen.getByRole("button", { name: /copy confighash/i })).toBeInTheDocument());
  });
});

/* ====================================================================== */
/*  Resizable grip + localStorage                                         */
/* ====================================================================== */

describe("ControllerDetail — Logs resizable grip (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
    localStorage.clear();
  });

  it("hydrates height from localStorage and clamps to the 900 max", async () => {
    const user = userEvent.setup();
    localStorage.setItem("varroa-controller-log-height", "2000");
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    const grip = screen.getByRole("separator", { name: /resize log pane/i });
    expect(grip).toHaveAttribute("aria-valuenow", "900");
    expect(screen.getByTestId("log-pane")).toHaveStyle({ height: "900px" });
  });

  it("ArrowUp grows by one line and commits the height to localStorage", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    const grip = screen.getByRole("separator", { name: /resize log pane/i });
    expect(grip).toHaveAttribute("aria-valuenow", "380");
    grip.focus();
    await user.keyboard("{ArrowUp}");
    expect(grip).toHaveAttribute("aria-valuenow", "401");
    expect(localStorage.getItem("varroa-controller-log-height")).toBe("401");
    await user.keyboard("{ArrowDown}");
    expect(grip).toHaveAttribute("aria-valuenow", "380");
    expect(localStorage.getItem("varroa-controller-log-height")).toBe("380");
  });

  it("Home and End jump to the bounds", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    const grip = screen.getByRole("separator", { name: /resize log pane/i });
    grip.focus();
    await user.keyboard("{End}");
    expect(grip).toHaveAttribute("aria-valuenow", "900");
    expect(localStorage.getItem("varroa-controller-log-height")).toBe("900");
    await user.keyboard("{Home}");
    expect(grip).toHaveAttribute("aria-valuenow", "160");
    expect(localStorage.getItem("varroa-controller-log-height")).toBe("160");
    expect(screen.getByTestId("log-pane")).toHaveStyle({ height: "160px" });
  });
});

/* ====================================================================== */
/*  Remote-cluster logs empty state (8.2)                                 */
/* ====================================================================== */

describe("ControllerDetail — Remote logs empty state (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("non-core cluster shows the core-only message and opens no stream", async () => {
    mockRouteCluster = "agent-1";
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    expect(
      screen.getByText("Logs are served only for controllers on the core cluster")
    ).toBeInTheDocument();
    const streamMock = (useEventStream as unknown as ReturnType<typeof vi.fn>);
    const logStreamUrls = streamMock.mock.calls
      .map((call: unknown[]) => call[0])
      .filter((url: unknown) => typeof url === "string" && url.includes("/logs"));
    expect(logStreamUrls).toHaveLength(0);
  });
});

/* ====================================================================== */
/*  Tile DOM order + lastReconciledAt formatting (BUG #386)              */
/* ====================================================================== */

describe("ControllerDetail — Tile DOM order & lastReconciledAt (BUG #386)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
    // Deterministic clock so age() output is stable
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-07-17T14:25:00Z"));
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders each tile's children in label → value → sub order", () => {
    setCtrl({
      jenkinsVersion: "2.462.1",
      observability: { summary: { totalJobs: 42, runningBuilds: 3 } },
      lastReconciledAt: "2026-07-17T14:22:01Z",
    });
    const { container } = renderWithProviders(<ControllerDetail />);
    const tilesContainer = container.querySelector('[class*="tiles"]');
    expect(tilesContainer).toBeTruthy();
    const tiles = tilesContainer!.children;
    expect(tiles.length).toBe(4);

    const expectedLabels = [
      "Jenkins version",
      "Jobs",
      "Running builds",
      "Last reconciled",
    ];

    for (let i = 0; i < tiles.length; i++) {
      const tile = tiles[i];
      expect(tile.children.length).toBe(3);
      // tileK (label) is first child
      expect(tile.children[0].textContent).toBe(expectedLabels[i]);
      // tileV (value) is second child (non-empty value present)
      expect(tile.children[1].textContent).toBeTruthy();
      // tileSub (sub) is third child (non-empty text present)
      expect(tile.children[2].textContent).toBeTruthy();
    }
  });

  it("formats lastReconciledAt as human-relative with title attribute for absolute time", () => {
    setCtrl({ lastReconciledAt: "2026-07-17T14:22:01Z" });
    renderWithProviders(<ControllerDetail />);
    // age("2026-07-17T14:22:01Z") with clock at 2026-07-17T14:25:00Z → ~179s → "2m ago"
    const relativeEl = screen.getByText("2m ago");
    expect(relativeEl).toBeInTheDocument();
    expect(relativeEl.tagName).toBe("SPAN");
    expect(relativeEl).toHaveAttribute(
      "title",
      new Date("2026-07-17T14:22:01Z").toLocaleString(),
    );
  });

  it("renders em-dash when lastReconciledAt is undefined", () => {
    setCtrl({ lastReconciledAt: undefined });
    renderWithProviders(<ControllerDetail />);
    // getByText("Last reconciled") returns the tileK div; its parent is the tile div
    const tile = screen.getByText("Last reconciled").parentElement!;
    expect(tile.children[1].textContent).toBe("—");
  });

  it("renders em-dash when lastReconciledAt is empty string", () => {
    setCtrl({ lastReconciledAt: "" });
    renderWithProviders(<ControllerDetail />);
    const tile = screen.getByText("Last reconciled").parentElement!;
    expect(tile.children[1].textContent).toBe("—");
  });
});

/* ====================================================================== */
/*  C2 — mite image staleness badge                                        */
/* ====================================================================== */

describe("ControllerDetail — Mite image staleness badge (C2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("renders 'stale' when miteImageStatus.stale is true", () => {
    setCtrl({ miteImageStatus: { image: "ghcr.io/mite:v1", stale: true } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/mite image/)).toBeInTheDocument();
    expect(screen.getByText("stale")).toBeInTheDocument();
  });

  it("renders 'current' when miteImageStatus.stale is false", () => {
    setCtrl({ miteImageStatus: { image: "ghcr.io/mite:v1", stale: false } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/mite image/)).toBeInTheDocument();
    expect(screen.getByText("current")).toBeInTheDocument();
  });

  it("omits the badge when miteImageStatus is absent", () => {
    // default mockControllerData has no miteImageStatus — confirm it's not rendered.
    setCtrl({});
    renderWithProviders(<ControllerDetail />);
    expect(screen.queryByText(/mite image/)).not.toBeInTheDocument();
    expect(screen.queryByText("stale")).not.toBeInTheDocument();
    expect(screen.queryByText("current")).not.toBeInTheDocument();
  });
});
