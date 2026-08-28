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
  return render(ui, {
    wrapper: ({ children }) => (
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>{children}</MemoryRouter>
      </QueryClientProvider>
    ),
  });
}

let mockRouteCluster = "core";
let mockRouteNamespace = "my-ns";
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ cluster: mockRouteCluster, namespace: mockRouteNamespace, name: "my-ctrl" }),
  };
});
afterEach(() => { mockRouteCluster = "core"; mockRouteNamespace = "my-ns"; });

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
/*  8.2 — State-aware Overview (the pipeline is the verdict)               */
/* ====================================================================== */

describe("ControllerDetail — State-aware Overview (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("disconnected mite renders the offline jenkins node, no verdict banner", () => {
    setCtrl({
      miteConnected: false,
      lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }] },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("mite disconnected")).toBeInTheDocument();
    expect(screen.queryByText(/Last apply failed/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Healthy — desired state converged/)).not.toBeInTheDocument();
  });

  it("failed apply names the failed sections in the apply node", () => {
    setCtrl({
      lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }, { name: "config", ok: true }] },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("rbac rejected")).toBeInTheDocument();
    expect(screen.getByText(/✗ rbac/)).toBeInTheDocument();
    expect(screen.getByText(/✓ config/)).toBeInTheDocument();
  });

  it("invalid attached bundle shows its error and Detach when canUpdate", () => {
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Invalid", errors: ["bad"] } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText("bundle-x").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText(/⛔ bad/)).toBeInTheDocument();
    const detachBtns = screen.getAllByRole("button", { name: /detach/i });
    expect(detachBtns.length).toBeGreaterThanOrEqual(1);
  });

  it("invalid attached bundle hides Detach when !canUpdate", () => {
    mockCanDo.mockImplementation((_p?: any, _ns?: string, r?: string, v?: string) => !(r === "controllers" && v === "update"));
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Invalid", errors: ["bad"] } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/⛔ bad/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /detach/i })).not.toBeInTheDocument();
  });

  it("drift no longer renders a verdict banner — the pipeline is the verdict", () => {
    setCtrl({ liveDrift: { detected: true, liveConfigHash: "abc" } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.queryByText(/Configuration drift detected/)).not.toBeInTheDocument();
    expect(screen.getAllByText("Bundle").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("Jenkins")).toBeInTheDocument();
  });

  it("healthy controller renders the delivery pipeline with a Connected pill", () => {
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText("Bundle").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("Apply")).toBeInTheDocument();
    expect(screen.getByText("Jenkins")).toBeInTheDocument();
    expect(screen.getByText("Connected")).toBeInTheDocument();
    expect(screen.queryByText(/Healthy — desired state converged/)).not.toBeInTheDocument();
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

  it("hibernate confirm shows exact body and fires hibernateController", async () => {
    const { hibernateController } = await import("../api/client");
    const user = userEvent.setup();
    setCtrl({ powerState: "Running" });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /hibernate/i }));
    expect(screen.getByText(/Scales the controller to zero/)).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /yes, hibernate/i }));
    expect(hibernateController).toHaveBeenCalled();
  });

  it("singular blast-radius with runningBuilds=1", async () => {
    const user = userEvent.setup();
    setCtrl({
      lastApplyResult: { succeeded: false, sections: [{ name: "x", ok: false }] },
      observability: { summary: { runningBuilds: 1 } },
    });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^restart rolls/i }));
    expect(screen.getByText(/1 running build will be terminated/)).toBeInTheDocument();
  });

  it("plural blast-radius with runningBuilds=3", async () => {
    const user = userEvent.setup();
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "x", ok: false }] }, observability: { summary: { runningBuilds: 3 } } });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^restart rolls/i }));
    expect(screen.getByText(/3 running builds will be terminated/)).toBeInTheDocument();
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
/*  Vitals + pipeline copy                                                 */
/* ====================================================================== */

describe("ControllerDetail — Vitals and pipeline copy (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    mockCanDo.mockImplementation(() => true);
  });

  it("renders the running version only inside the Jenkins node", () => {
    setCtrl({ jenkinsVersion: "2.462.1", version: "2.462.1", jenkinsHealth: "Unknown" });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("2.462.1 · Unknown")).toBeInTheDocument();
    // version no longer appears in the header meta row
    expect(screen.queryByText(/version 2\.462\.1/i)).not.toBeInTheDocument();
  });

  it("keeps the running version when the desired version differs (no drift banner)", () => {
    setCtrl({ jenkinsVersion: "2.461.0", version: "2.462.1" });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/2\.461\.0 · /)).toBeInTheDocument();
    expect(screen.queryByText(/desired 2.462.1/)).not.toBeInTheDocument();
  });

  it("shows job count in vitals when mite connected", () => {
    setCtrl({ miteConnected: true, observability: { summary: { totalJobs: 42 } } });
    const { container } = renderWithProviders(<ControllerDetail />);
    const vitals = container.querySelector('[class*="vitals"]');
    expect(vitals).toBeTruthy();
    expect(vitals!.textContent).toContain("42");
    expect(vitals!.textContent).toContain("jobs");
  });

  it("shows metrics unavailable when mite disconnected", () => {
    setCtrl({ miteConnected: false });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/metrics unavailable — mite offline/)).toBeInTheDocument();
  });

  it("shows 'idle' when mite connected and 0 running builds", () => {
    setCtrl({ miteConnected: true, observability: { summary: { runningBuilds: 0 } } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("idle")).toBeInTheDocument();
  });

  it("shows 'in flight' when builds > 0", () => {
    setCtrl({ miteConnected: true, observability: { summary: { runningBuilds: 2 } } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("in flight")).toBeInTheDocument();
  });

  it("shows reconciliation interval and mode in the apply sub-line", () => {
    setCtrl({
      reconciliationPolicy: { interval: "30s", mode: "automatic" },
      lastApplyResult: { succeeded: true, hash: "abc", timestamp: "2026-01-01T00:00:00Z", sections: [] },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText(/automatic · every 30s/).length).toBeGreaterThanOrEqual(1);
  });

  it("labels the cert expiry 'expires' and renders the correct local calendar day", () => {
    // certExpiry arrives as a date-only string ("2006-01-02"). It must render
    // the expiry date in the viewer's local timezone — the UTC-midnight
    // round-trip of `new Date("2026-08-20").toLocaleDateString()` shifts a
    // date-only parse back one day in any timezone west of UTC.
    const correct = new Date(2026, 7, 20).toLocaleDateString();
    const utcMidnight = new Date("2026-08-20").toLocaleDateString();
    setCtrl({ certExpiry: "2026-08-20" });
    const { container } = renderWithProviders(<ControllerDetail />);
    const vitals = container.querySelector('[class*="vitals"]');
    expect(vitals).toBeTruthy();
    expect(vitals!.textContent).toContain(`cert expires ${correct}`);
    expect(vitals!.textContent).not.toContain("renews");
    if (correct !== utcMidnight) {
      // Where the UTC parse would show the previous day (negative-offset
      // timezones), assert that wrong-day rendering is absent too.
      expect(vitals!.textContent).not.toContain(`cert expires ${utcMidnight}`);
    }
  });
});

/* ====================================================================== */
/*  Header path renders the real route namespace (BUG #484)               */
/* ====================================================================== */

describe("ControllerDetail — Header path namespace (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("renders the actual route namespace in the header path, not a hardcoded literal", () => {
    // Deliberately non-"varroa" namespace: the header must show the real
    // namespace from the route, not a hardcoded "varroa" string.
    mockRouteNamespace = "tools";
    const { container } = renderWithProviders(<ControllerDetail />);
    const path = container.querySelector('[class*="path"]');
    expect(path).toBeTruthy();
    expect(path!.textContent).toContain("core / tools");
    expect(path!.textContent).not.toContain("varroa");
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

  it("renders the delivery pipeline, vitals and Spec card on Overview", () => {
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText("Bundle").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("Apply")).toBeInTheDocument();
    expect(screen.getByText("Jenkins")).toBeInTheDocument();
    expect(screen.getByText("Spec")).toBeInTheDocument();
  });

  it("BundleCard renders on Overview when canUpdate", () => {
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Ready" } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText("bundle-x").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByRole("button", { name: "Change" })).toBeInTheDocument();
  });

  it("BundleCard read-only without canUpdate (no Change/Detach)", () => {
    mockCanDo.mockImplementation((_p?: any, _ns?: string, r?: string, v?: string) => !(r === "controllers" && v === "update"));
    setCtrl({ composedBundleRef: { name: "bundle-x" } });
    setBundles([{ metadata: { name: "bundle-x" }, status: { phase: "Ready" } }]);
    renderWithProviders(<ControllerDetail />);
    expect(screen.getAllByText("bundle-x").length).toBeGreaterThanOrEqual(1);
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
    const { wakeController } = await import("../api/client");
    const user = userEvent.setup();
    setCtrl({ hibernated: true, phase: "Hibernated" });
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByRole("button", { name: /Wake/ }));
    expect(wakeController).toHaveBeenCalled();
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

  it("approve-restart only: sees reload/restart approval but no Reprovision or header menu", async () => {
    mockCanDo.mockImplementation((_p?: any, _ns?: string, r?: string, v?: string) => r !== "controllers" || v === "approve-restart");
    setCtrl({ pendingRestart: { detectedAt: new Date().toISOString(), desiredStateHash: "abc", changes: ["jcasc"] } });
    renderWithProviders(<ControllerDetail />);
    // No header menu (needs manage)
    expect(screen.queryByRole("button", { name: /more actions/i })).not.toBeInTheDocument();
    // Approve-restart surfaces the reload/restart actions on the pending banner
    expect(screen.getByText(/Reload Configuration/)).toBeInTheDocument();
    expect(screen.getByText(/Safe Restart/)).toBeInTheDocument();
    // Reprovision (manage) is not available anywhere
    expect(screen.queryByText(/Reprovision/)).not.toBeInTheDocument();
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
/*  Apply node sections                                                   */
/* ====================================================================== */

describe("ControllerDetail — Apply node sections (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("shows ok and failed section chips in the apply node", () => {
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }, { name: "config", ok: true }] } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/✓ config/)).toBeInTheDocument();
    expect(screen.getByText(/✗ rbac/)).toBeInTheDocument();
  });

  it("shows error text from backend", () => {
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }] } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/denied/)).toBeInTheDocument();
  });

  it("shows fallback string when error absent", () => {
    setCtrl({ lastApplyResult: { succeeded: false, sections: [{ name: "rbac", ok: false }] } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/rejected, no message returned/)).toBeInTheDocument();
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
/*  Diagnostics stream (merged activity + logs)                           */
/* ====================================================================== */

describe("ControllerDetail — Diagnostics stream (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("renders the merged stream under Diagnostics with filter chips", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);
    await user.click(screen.getByText(/Diagnostics/));
    expect(screen.getByText(/Stream/)).toBeInTheDocument();
    for (const chip of ["All", "Operator", "Mite", "Jenkins", "User", "Logs"]) {
      expect(screen.getAllByText(new RegExp(chip)).length).toBeGreaterThanOrEqual(1);
    }
  });

  it("shows an inline reconnecting row when the log stream errors", async () => {
    const user = userEvent.setup();
    vi.mocked(useEventStream).mockReturnValue({
      lastEvent: null,
      readyState: "closed",
      error: new Error("403 Forbidden"),
    });
    try {
      renderWithProviders(<ControllerDetail />);
      await user.click(screen.getByText(/Diagnostics/));
      expect(screen.getByText(/log stream reconnecting…/)).toBeInTheDocument();
    } finally {
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("'Logs' chip shows real log entries (no source filter) and hides activity", () => {
    // Stable references: ControllerStream appends lines on a [lastEvent]
    // effect and useActivityFeed ingests on [lastEvent] too, so each simulated
    // "current event" must be constructed ONCE and the same object returned on
    // every mock invocation. A fresh object literal per render would make those
    // effects fire every render and infinite-loop.
    const activityState = {
      lastEvent: {
        type: "activity",
        data: {
          timestamp: "2024-01-01T00:00:00Z",
          type: "info",
          source: "operator",
          message: "operator activity line",
          cluster: "core",
          namespace: "my-ns",
          controller: "my-ctrl",
        },
      },
      readyState: "open",
      error: null,
    };
    // Log frame source is "jenkins" — never literally "logs".
    const logState = {
      lastEvent: { type: "log", data: { timestamp: "2024-01-01T00:00:01Z", source: "jenkins", message: "jenkins log line" } },
      readyState: "open",
      error: null,
    };
    try {
      vi.mocked(useEventStream).mockImplementation((_baseUrl, scope) => (scope === "activity" ? activityState : logState));
      renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      // "All" (default) mixes activity + log lines.
      expect(screen.getByText(/operator activity line/)).toBeInTheDocument();
      expect(screen.getByText(/jenkins log line/)).toBeInTheDocument();
      // Selecting "Logs" must keep the real log entry and drop the activity entry.
      fireEvent.click(screen.getByRole("button", { name: "Logs" }));
      expect(screen.getByText(/jenkins log line/)).toBeInTheDocument();
      expect(screen.queryByText(/operator activity line/)).not.toBeInTheDocument();
    } finally {
      // Don't let this custom implementation leak into later tests.
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("with 'Logs' selected, live reflects only the log stream (activity open must not mask a dead log feed)", () => {
    // Stable per-scope states, constructed once (see note on the 'Logs' chip
    // test above) — never return a fresh object per mock invocation.
    const activityUp = { lastEvent: null, readyState: "open", error: null };
    const logDown = { lastEvent: null, readyState: "closed", error: null };
    try {
      // Activity connection UP, log connection DOWN.
      vi.mocked(useEventStream).mockImplementation((_baseUrl, scope) => (scope === "activity" ? activityUp : logDown));
      const { container } = renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      // "All" chip: either connection open counts as live.
      expect(container.querySelector('[class*="liveInd"]')!.textContent).toContain("live");
      // "Logs" chip: only the (dead) log stream counts -> connecting…
      fireEvent.click(screen.getByRole("button", { name: "Logs" }));
      expect(container.querySelector('[class*="liveInd"]')!.textContent).toContain("connecting…");
    } finally {
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("with 'Logs' selected, live is shown when the log stream is open even if activity is down", () => {
    // Stable per-scope states, constructed once — never a fresh object per call.
    const activityDown = { lastEvent: null, readyState: "closed", error: null };
    const logUp = { lastEvent: null, readyState: "open", error: null };
    try {
      // Activity connection DOWN, log connection UP.
      vi.mocked(useEventStream).mockImplementation((_baseUrl, scope) => (scope === "activity" ? activityDown : logUp));
      const { container } = renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      fireEvent.click(screen.getByRole("button", { name: "Logs" }));
      expect(container.querySelector('[class*="liveInd"]')!.textContent).toContain("live");
    } finally {
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("does not dedupe a genuinely repeated log line outside the reconnect settle window", () => {
    const nowSpy = vi.spyOn(Date, "now");
    // Stable per-phase event objects: ControllerStream's effect depends on
    // [lastEvent], so each log event must keep one identity while it is the
    // "current" event and change identity only when a genuinely new event
    // arrives — a fresh object per render would re-fire the effect forever.
    const activityState = { lastEvent: null, readyState: "open", error: null };
    const logEventA = { type: "log", data: { timestamp: "2024-01-01T00:00:00Z", source: "jenkins", message: "jenkins heartbeat" } };
    const logEventB = { type: "log", data: { timestamp: "2024-01-01T00:00:06Z", source: "jenkins", message: "jenkins heartbeat" } };
    let currentLogEvent = logEventA;
    try {
      nowSpy.mockReturnValue(1_700_000_000_000); // arbitrary base "now"
      const streamMock = vi.mocked(useEventStream);
      streamMock.mockImplementation((_baseUrl, scope) =>
        scope === "activity"
          ? activityState
          : { lastEvent: currentLogEvent, readyState: "open", error: null },
      );
      const { rerender } = renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      expect(screen.getByText(/jenkins heartbeat/)).toBeInTheDocument();
      // Step beyond the 5s post-(re)connect settle window, then deliver the
      // same line again — steady-state repetition is real output, not replay.
      nowSpy.mockReturnValue(1_700_000_000_000 + 6_000); // simulate 6s later, outside the settle window
      currentLogEvent = logEventB;
      rerender(<ControllerDetail />);
      expect(screen.getAllByText(/jenkins heartbeat/)).toHaveLength(2);
    } finally {
      nowSpy.mockRestore();
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("still dedupes an identical line replayed inside the reconnect settle window", () => {
    const nowSpy = vi.spyOn(Date, "now");
    // Stable per-phase event objects — see the sibling 'does not dedupe' test.
    // Both frames carry a timestamp BEFORE the (re)connect moment
    // (connectedAtRef = Date.now() = 1_700_000_000_000), the signature of a
    // re-tailed tail line: pre-connect timestamps inside the settle window
    // with a matching (source, message) are treated as replay and dropped.
    const activityState = { lastEvent: null, readyState: "open", error: null };
    const logEventA = { type: "log", data: { timestamp: "2023-01-01T00:00:00Z", source: "mite", message: "mite status snapshot" } };
    const logEventB = { type: "log", data: { timestamp: "2023-01-01T00:00:01Z", source: "mite", message: "mite status snapshot" } };
    let currentLogEvent = logEventA;
    try {
      nowSpy.mockReturnValue(1_700_000_000_000); // arbitrary base "now"
      const streamMock = vi.mocked(useEventStream);
      streamMock.mockImplementation((_baseUrl, scope) =>
        scope === "activity"
          ? activityState
          : { lastEvent: currentLogEvent, readyState: "open", error: null },
      );
      const { rerender } = renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      expect(screen.getByText(/mite status snapshot/)).toBeInTheDocument();
      // Same pre-connect line arrives again while still inside the 5s settle
      // window -> treated as a reconnect tail replay and dropped.
      nowSpy.mockReturnValue(1_700_000_000_000 + 1_000); // still inside the 5s settle window
      currentLogEvent = logEventB;
      rerender(<ControllerDetail />);
      expect(screen.getAllByText(/mite status snapshot/)).toHaveLength(1);
    } finally {
      nowSpy.mockRestore();
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("does not dedupe a repeated line inside the settle window when its timestamp is at/after connection (new output, not replay)", () => {
    // Regression guard for the recurring finding: within the settle window,
    // client-receipt-order alone (a matching recent source+message) must not
    // classify a line as replay. If the line's OWN timestamp is at/after the
    // connection, it is genuinely new output (e.g. a heartbeat emitted right
    // after a reconnect) and must be kept even though it repeats a very
    // recent message.
    const nowSpy = vi.spyOn(Date, "now");
    const activityState = { lastEvent: null, readyState: "open", error: null };
    // Timestamps (2024) are AFTER the connection time (1_700_000_000_000).
    const logEventA = { type: "log", data: { timestamp: "2024-01-01T00:00:00Z", source: "jenkins", message: "jenkins heartbeat" } };
    const logEventB = { type: "log", data: { timestamp: "2024-01-01T00:00:00Z", source: "jenkins", message: "jenkins heartbeat" } };
    let currentLogEvent = logEventA;
    try {
      nowSpy.mockReturnValue(1_700_000_000_000); // arbitrary base "now"
      const streamMock = vi.mocked(useEventStream);
      streamMock.mockImplementation((_baseUrl, scope) =>
        scope === "activity"
          ? activityState
          : { lastEvent: currentLogEvent, readyState: "open", error: null },
      );
      const { rerender } = renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      expect(screen.getByText(/jenkins heartbeat/)).toBeInTheDocument();
      // Still inside the 5s settle window, same source+message again, but its
      // timestamp is at/after the connection -> genuinely new output, kept.
      nowSpy.mockReturnValue(1_700_000_000_000 + 1_000); // still inside the 5s settle window
      currentLogEvent = logEventB;
      rerender(<ControllerDetail />);
      expect(screen.getAllByText(/jenkins heartbeat/)).toHaveLength(2);
    } finally {
      nowSpy.mockRestore();
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("identical timestamp+message log lines render as distinct rows with no key collision", () => {
    const nowSpy = vi.spyOn(Date, "now");
    // Two log frames with the SAME timestamp and SAME message, delivered
    // outside the 5s reconnect settle window so neither is treated as replay.
    // Their entries keys must stay unique (index included) — a key of just
    // `l-${timestamp}-${message}` would collide here and trip React's
    // "two children with the same key" warning.
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const activityState = { lastEvent: null, readyState: "open", error: null };
    const logEventA = { type: "log", data: { timestamp: "2024-01-01T00:00:00Z", source: "jenkins", message: "identical burst line" } };
    const logEventB = { type: "log", data: { timestamp: "2024-01-01T00:00:00Z", source: "jenkins", message: "identical burst line" } };
    let currentLogEvent = logEventA;
    try {
      nowSpy.mockReturnValue(1_700_000_000_000); // arbitrary base "now"
      const streamMock = vi.mocked(useEventStream);
      streamMock.mockImplementation((_baseUrl, scope) =>
        scope === "activity"
          ? activityState
          : { lastEvent: currentLogEvent, readyState: "open", error: null },
      );
      const { rerender } = renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      expect(screen.getByText(/identical burst line/)).toBeInTheDocument();
      // Step beyond the 5s settle window and deliver the same line again —
      // same timestamp and message as the first, so only an indexed key keeps
      // the two entries distinct.
      nowSpy.mockReturnValue(1_700_000_000_000 + 6_000);
      currentLogEvent = logEventB;
      rerender(<ControllerDetail />);
      expect(screen.getAllByText(/identical burst line/)).toHaveLength(2);
      const keyWarnings = errorSpy.mock.calls.filter(
        ([msg]) => typeof msg === "string" && /same key|non-unique keys?/i.test(msg),
      );
      expect(keyWarnings).toHaveLength(0);
    } finally {
      nowSpy.mockRestore();
      errorSpy.mockRestore();
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });

  it("activity events sharing timestamp/type/message but different sources render as distinct rows with no key collision", () => {
    // Two activity events with the SAME timestamp, type, and message but
    // DIFFERENT sources. Their stream-entry keys must include the source —
    // a key of just `a-${timestamp}-${type}-${message}` would collide and
    // trip React's "two children with the same key" warning.
    const errorSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    // Stable per-scope states / event objects (see the sibling 'Logs' chip
    // test): never return a fresh object per mock invocation.
    const logState = { lastEvent: null, readyState: "open", error: null };
    const shared = {
      timestamp: "2024-01-01T00:00:00Z",
      type: "info",
      message: "shared activity message",
      cluster: "core",
      namespace: "my-ns",
      controller: "my-ctrl",
    };
    const activityEventA = { type: "activity", data: { ...shared, source: "operator" } };
    const activityEventB = { type: "activity", data: { ...shared, source: "mite" } };
    let currentActivityEvent = activityEventA;
    try {
      const streamMock = vi.mocked(useEventStream);
      streamMock.mockImplementation((_baseUrl, scope) =>
        scope === "activity"
          ? { lastEvent: currentActivityEvent, readyState: "open", error: null }
          : logState,
      );
      const { rerender } = renderWithProviders(<ControllerDetail />);
      fireEvent.click(screen.getByText(/Diagnostics/));
      expect(screen.getAllByText(/shared activity message/)).toHaveLength(1);
      // Deliver a second activity event from a different source, otherwise
      // identical — only a source-qualified key keeps the two entries apart.
      currentActivityEvent = activityEventB;
      rerender(<ControllerDetail />);
      expect(screen.getAllByText(/shared activity message/)).toHaveLength(2);
      const keyWarnings = errorSpy.mock.calls.filter(
        ([msg]) => typeof msg === "string" && /same key|non-unique keys?/i.test(msg),
      );
      expect(keyWarnings).toHaveLength(0);
    } finally {
      errorSpy.mockRestore();
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
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

  it("non-core: Logs chip is disabled, every chip exposes aria-pressed, and live reads 'logs unavailable'", () => {
    const { container, rerender } = renderWithProviders(<ControllerDetail />);
    fireEvent.click(screen.getByText(/Diagnostics/));
    // Start on the core cluster with the Logs chip selected…
    fireEvent.click(screen.getByRole("button", { name: "Logs" }));

    // …then the controller moves to a non-core cluster: the Logs chip becomes
    // disabled (cannot be re-selected) and the live indicator reflects that
    // logs are unavailable instead of showing "connecting…" forever.
    mockRouteCluster = "agent-1";
    rerender(<ControllerDetail />);

    const chips = Array.from(container.querySelectorAll('[class*="fchip"]')) as HTMLButtonElement[];
    expect(chips.length).toBe(6);
    for (const chip of chips) {
      expect(chip).toHaveAttribute("aria-pressed");
    }

    expect(screen.getByRole("button", { name: "Logs" })).toBeDisabled();
    expect(container.querySelector('[class*="liveInd"]')!.textContent).toContain("logs unavailable");
  });
});

/* ====================================================================== */
/*  Tile DOM order + lastReconciledAt formatting (BUG #386)              */
/* ====================================================================== */

describe("ControllerDetail — Vitals DOM (8.2)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setCtrl({});
    mockCanDo.mockImplementation(() => true);
  });

  it("renders a single vitals line with jobs and running builds", () => {
    setCtrl({ observability: { summary: { totalJobs: 42, runningBuilds: 3 } } });
    const { container } = renderWithProviders(<ControllerDetail />);
    const vitals = container.querySelector('[class*="vitals"]');
    expect(vitals).toBeTruthy();
    expect(vitals!.textContent).toContain("42");
    expect(vitals!.textContent).toContain("3");
    expect(vitals!.textContent).toContain("jobs");
    expect(vitals!.textContent).toContain("running builds");
    expect(vitals!.textContent).toContain("in flight");
  });

  it("renders an em-dash for jobs when the mite has not reported a summary", () => {
    setCtrl({ observability: undefined });
    const { container } = renderWithProviders(<ControllerDetail />);
    const vitals = container.querySelector('[class*="vitals"]');
    expect(vitals!.textContent).toContain("—");
  });

  it("flags a stale observability cache in the vitals line", () => {
    setCtrl({
      observability: {
        freshness: { stale: true, observedAt: "2024-01-01T00:00:00Z" },
        summary: { totalJobs: 1 },
      },
    });
    const { container } = renderWithProviders(<ControllerDetail />);
    const vitals = container.querySelector('[class*="vitals"]');
    expect(vitals).toBeTruthy();
    expect(vitals!.textContent).toContain("obs stale");
  });

  it("omits the obs indicator before the mite has ever reported", () => {
    setCtrl({ observability: { freshness: { stale: false }, summary: { totalJobs: 1 } } });
    const { container } = renderWithProviders(<ControllerDetail />);
    const vitals = container.querySelector('[class*="vitals"]');
    expect(vitals).toBeTruthy();
    expect(vitals!.textContent).not.toContain("obs ");
  });

  it("shows obs current when only the mite TTL is present", () => {
    setCtrl({
      observability: { freshness: { stale: false, miteTTL: 180 }, summary: { totalJobs: 1 } },
    });
    const { container } = renderWithProviders(<ControllerDetail />);
    const vitals = container.querySelector('[class*="vitals"]');
    expect(vitals).toBeTruthy();
    expect(vitals!.textContent).toContain("obs current");
  });

  it("surfaces observability warnings under the vitals line", () => {
    setCtrl({ observability: { warnings: [{ message: "jenkins api: cached metrics may be stale" }] } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/cached metrics may be stale/)).toBeInTheDocument();
  });

  it("surfaces per-source observability errors under the vitals line", () => {
    setCtrl({
      observability: {
        sources: [
          { provider: "jenkins-api", status: "unavailable", error: "connection refused" },
          { provider: "prometheus", status: "integrated", error: undefined },
        ],
      },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/jenkins-api: connection refused/)).toBeInTheDocument();
  });

  it("does not surface non-erroring sources under the vitals line", () => {
    setCtrl({
      observability: {
        sources: [
          { provider: "prometheus", status: "degraded" },
          { provider: "jenkins-api", status: "unavailable", error: "no endpoint" },
        ],
      },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/no endpoint/)).toBeInTheDocument();
    expect(screen.queryByText(/prometheus:/)).not.toBeInTheDocument();
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

  it("renders 'image stale' when miteImageStatus.stale is true", () => {
    setCtrl({ miteImageStatus: { image: "ghcr.io/mite:v1", stale: true } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/image stale/)).toBeInTheDocument();
  });

  it("renders 'image current' when miteImageStatus.stale is false", () => {
    setCtrl({ miteImageStatus: { image: "ghcr.io/mite:v1", stale: false } });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/image current/)).toBeInTheDocument();
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
