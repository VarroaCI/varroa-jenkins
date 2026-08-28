import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent, within, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import ControllerDetail from "./ControllerDetail";
import { useEventStream } from "../hooks/useEventStream";
import { restartController } from "../api/client";
import type { PodOverrides, ResourceOverlay, ProbesSpec, ControllerSpec } from "../types";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false } },
});

let openConfiguration = false;

function renderWithProviders(ui: React.ReactElement) {
  const result = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>
  );
  if (openConfiguration) fireEvent.click(screen.getAllByText(/Configuration/)[0]);
  return result;
}

beforeEach(() => { openConfiguration = false; mockToast.mockReset(); });

// MemoryRouter + useParams is handled via MemoryRouter initialEntries
// But the component uses useParams directly, so mock it
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ cluster: "core", namespace: "my-ns", name: "my-ctrl" }),
  };
});

vi.mock("../hooks/useClusters", () => ({ useClusters: () => ({ data: [{name:"core",core:true,healthy:true,lastHeartbeat:"2025-01-01T00:00:00Z",operatorVersion:"1.0",k8sVersion:"1.28",controllerCount:5,connectedCount:4}], isLoading: false, isError: false }), coreOf: (c: unknown[]) => c?.find((c2: any) => c2.core), clusterQuery: (c: string) => (c && c !== "core" ? `?cluster=${c}` : "") }));

// Mock updateController
const mockUpdateController = vi.fn();
const mockPreviewControllerOverlay = vi.fn();
vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    updateController: (...args: unknown[]) => mockUpdateController(...args),
    previewControllerOverlay: (...args: unknown[]) => mockPreviewControllerOverlay(...args),
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
    // parsePreflightChecks is a pure helper — use the real implementation.
    parsePreflightChecks: actual.parsePreflightChecks,
  };
});

// Mock useController hook
const mockControllerData = {
  name: "my-ctrl",
  namespace: "my-ns",
  cluster: "core",
  phase: "Connected",
  endpoint: "https://builds.example.com",
  version: "2.462.1",
  jenkinsVersion: undefined as string | undefined,
  miteConnected: true,
  composedBundleRef: null as { name: string } | null,
  powerState: undefined as string | undefined,
  reconciliationPolicy: undefined as Record<string, string> | undefined,
};

const mockUseController = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useController: () => mockUseController(),
}));

// Mock useComposedBundles
const mockUseComposedBundles = vi.fn();
vi.mock("../hooks/useCatalog", () => ({
  useComposedBundles: () => mockUseComposedBundles(),
}));

// Mock useEventStream
vi.mock("../hooks/useEventStream", () => {
  const mock = vi.fn(() => ({ lastEvent: null, readyState: "closed", error: null }));
  return { useEventStream: mock, __esModule: true };
});

// Mock useAuth
vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: { global: {}, scopes: [] } }),
}));

// Mock usePermissions — canDoInNamespace is a fn so tests can toggle admin gating.
const mockCanDo = vi.fn(
  (_perms?: unknown, _ns?: string, _resource?: string, _verb?: string): boolean => true
);
vi.mock("../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: { global: {}, scopes: [] } }),
  canDoInNamespace: (perms: unknown, ns: string, resource: string, verb: string) => mockCanDo(perms, ns, resource, verb),
}));

// Mock Toast — shared mock so tests can assert on what the surfaces toast
// (a blocked removal must replace the unqualified success, never accompany it).
const mockToast = vi.fn();
vi.mock("../components/Toast", () => ({
  useToast: () => ({ toast: mockToast }),
}));

// Mock BundleSelector
vi.mock("../components/BundleSelector", () => ({
  BundleSelector: ({
    value,
    onChange,
  }: {
    namespace: string;
    value: string | null;
    onChange: (name: string | null) => void;
  }) => (
    <div data-testid="bundle-selector">
      <span data-testid="bundle-value">{value ?? "(none)"}</span>
      <button data-testid="select-bundle-x" onClick={() => onChange("bundle-x")}>
        Select Bundle X
      </button>
      <button data-testid="select-bundle-invalid" onClick={() => onChange("bundle-invalid")}>
        Select Invalid Bundle
      </button>
      <button data-testid="clear-bundle" onClick={() => onChange(null)}>
        Clear
      </button>
    </div>
  ),
  BundleHealthBadge: ({ phase }: { phase?: string }) => (
    <span data-testid={`badge-${phase ?? "none"}`}>[{phase ?? "no-phase"}]</span>
  ),
}));

// Helpers
function setControllerData(
  overrides: Partial<
    typeof mockControllerData & { podOverrides?: PodOverrides; resourceOverlay?: ResourceOverlay; probes?: ProbesSpec; spec?: ControllerSpec }
  >,
) {
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

describe("ControllerDetail — Bundle management", () => {
  beforeEach(() => {
    openConfiguration = false;
    mockUpdateController.mockReset();
    setControllerData({ composedBundleRef: null });
    setBundles([
      {
        metadata: { name: "bundle-x" },
        spec: { displayName: "Bundle X" },
        status: { phase: "Ready" as const },
      },
      {
        metadata: { name: "bundle-invalid" },
        spec: {},
        status: {
          phase: "Invalid" as const,
          errors: ["Missing required item"],
        },
      },
    ]);
  });

  it("shows attach UI when no bundle is attached", () => {
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByTestId("bundle-selector")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /attach bundle/i })).toBeInTheDocument();
  });

  it("attaches a bundle via PATCH when selected and confirmed", async () => {
    mockUpdateController.mockResolvedValueOnce({});
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByTestId("select-bundle-x"));
    await user.click(screen.getByRole("button", { name: /attach bundle/i }));

    await waitFor(() => {
      expect(mockUpdateController).toHaveBeenCalledWith(
        "core",
        "my-ctrl",
        "my-ns",
        { spec: { composedBundleRef: { name: "bundle-x" } } },
      );
    });
  });

  it("shows attached bundle name with health badge and change/detach buttons", () => {
    setControllerData({ composedBundleRef: { name: "bundle-x" } });
    renderWithProviders(<ControllerDetail />);

    expect(screen.getAllByText("bundle-x").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByTestId("badge-Ready")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Change" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Detach" })).toBeInTheDocument();
  });

  it("changes to a new bundle via selector and confirm", async () => {
    mockUpdateController.mockResolvedValueOnce({});
    setControllerData({ composedBundleRef: { name: "bundle-x" } });
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    // Click Change to show selector
    await user.click(screen.getByRole("button", { name: "Change" }));
    // Select new bundle
    await user.click(screen.getByTestId("select-bundle-invalid"));
    // Confirm
    await user.click(screen.getByRole("button", { name: /confirm change/i }));

    await waitFor(() => {
      expect(mockUpdateController).toHaveBeenCalledWith(
        "core",
        "my-ctrl",
        "my-ns",
        { spec: { composedBundleRef: { name: "bundle-invalid" } } },
      );
    });
  });

  it("detaches a bundle via PATCH clearing composedBundleRef", async () => {
    mockUpdateController.mockResolvedValueOnce({});
    setControllerData({ composedBundleRef: { name: "bundle-x" } });
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: "Detach" }));

    await waitFor(() => {
      expect(mockUpdateController).toHaveBeenCalledWith(
        "core",
        "my-ctrl",
        "my-ns",
        { spec: { composedBundleRef: null } },
      );
    });
  });

  it("surfaces a non-blocking notice when a detach is retained by another manager", async () => {
    // The detach PATCH sends composedBundleRef: null, but the server reports
    // the removal did not take effect (another manager owns the field). The
    // save SUCCEEDED — so this must be a notice, not an error.
    mockUpdateController.mockResolvedValueOnce({
      unappliedRemovals: [{ field: "spec.composedBundleRef" }],
    });
    setControllerData({ composedBundleRef: { name: "bundle-x" } });
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: "Detach" }));

    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith(
        expect.stringContaining("spec.composedBundleRef could not be removed"),
      ),
    );
    // The unqualified success is replaced, not shown alongside it.
    expect(mockToast).not.toHaveBeenCalledWith("Bundle detached");
  });

  it("reports an unqualified success when no removal was blocked", async () => {
    mockUpdateController.mockResolvedValueOnce({});
    setControllerData({ composedBundleRef: { name: "bundle-x" } });
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: "Detach" }));

    await waitFor(() => expect(mockToast).toHaveBeenCalledWith("Bundle detached"));
    expect(mockToast).not.toHaveBeenCalledWith(expect.stringMatching(/could not be removed/));
  });

  it("warns when attaching an Invalid bundle", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    // Select the invalid bundle via the mocked selector
    await user.click(screen.getByTestId("select-bundle-invalid"));

    // Warning about Invalid bundle should appear
    expect(screen.getByText(/This bundle is Invalid/)).toBeInTheDocument();
    expect(screen.getByText(/Missing required item/)).toBeInTheDocument();

    // Attach button should still be enabled (Invalid bundles are allowed)
    expect(screen.getByRole("button", { name: /attach bundle/i })).not.toBeDisabled();
  });

  it("shows errors/warnings for an attached non-Ready bundle", () => {
    setControllerData({ composedBundleRef: { name: "bundle-invalid" } });
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByTestId("badge-Invalid")).toBeInTheDocument();
    expect(screen.getByText(/Missing required item/)).toBeInTheDocument();
  });
});

// ---- Extended tests ----

describe("ControllerDetail — Loading state", () => {
  it("shows a loading banner when controller data is loading", () => {
    mockUseController.mockReturnValue({
      data: null,
      isLoading: true,
      error: null,
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Loading controller...")).toBeInTheDocument();
  });
});

describe("ControllerDetail — Error state", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
  });

  it("shows error banner when controller fetch fails", () => {
    mockUseController.mockReturnValue({
      data: null,
      isLoading: false,
      error: { message: "Controller not found" },
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Varroa could not load this page.")).toBeInTheDocument();
  });

  it('shows "Controller not found" when data is null without error', () => {
    mockUseController.mockReturnValue({
      data: null,
      isLoading: false,
      error: null,
    });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByText("We could not find that page.")).toBeInTheDocument();
  });
});

describe("ControllerDetail — Tab navigation", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setControllerData({});
  });

  it("renders three workspace tabs and lazy-mounts only Overview", () => {
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Overview/)).toBeInTheDocument();
    expect(screen.getAllByText(/Configuration/)[0]).toBeInTheDocument();
    expect(screen.getByText(/Diagnostics/)).toBeInTheDocument();
    expect(screen.queryByText(/Observability/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Plugins/)).not.toBeInTheDocument();
    expect(screen.queryByTestId("configuration-tab")).not.toBeInTheDocument();
    expect(screen.queryByTestId("diagnostics-tab")).not.toBeInTheDocument();
  });

  it("shows Overview tab by default", () => {
    renderWithProviders(<ControllerDetail />);
    // Overview tab content — the Spec card header
    expect(screen.getByText("Spec")).toBeInTheDocument();
  });

  it("shows endpoint namespace reconciliation in Spec card", () => {
    setControllerData({ version: "2.555", jenkinsVersion: "2.541.1" });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Endpoint")).toBeInTheDocument();
    expect(screen.getByText("Reconciliation")).toBeInTheDocument();
  });

  it("shows namespace in Spec card", () => {
    setControllerData({ version: "2.555", jenkinsVersion: "2.555.1" });
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText("Namespace")).toBeInTheDocument();
  });
});

describe("ControllerDetail — Power state controls", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
  });

  it("shows Reprovision and Restart buttons", async () => {
    const user = userEvent.setup();
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: /more actions/i }));
    expect(screen.getByRole("menuitem", { name: /reprovision/i })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: /^restart rolls/i })).toBeInTheDocument();
  });

  it("shows Power Off button when controller is running", async () => {
    const user = userEvent.setup();
    setControllerData({ powerState: "Running" });
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: /more actions/i }));
    expect(screen.getByRole("menuitem", { name: /^power off stops/i })).toBeInTheDocument();
  });

  it("shows Power On button when controller is stopped", () => {
    setControllerData({ powerState: "Stopped", phase: "Stopped" });
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText(/Power On/)).toBeInTheDocument();
    expect(screen.queryByText(/Power Off/)).not.toBeInTheDocument();
  });

  it("shows restart confirmation when Restart is clicked", async () => {
    const user = userEvent.setup();
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^restart rolls/i }));

    expect(screen.getByText(/Yes, restart/)).toBeInTheDocument();
    expect(screen.getByText(/Cancel/)).toBeInTheDocument();
  });

  it("shows power off confirmation when Power Off is clicked", async () => {
    const user = userEvent.setup();
    setControllerData({ powerState: "Running" });
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^power off stops/i }));

    expect(screen.getByText(/Yes, power off/)).toBeInTheDocument();
  });

  it("keeps the confirm dialog open with a disabled, in-progress button while the restart request is outstanding", async () => {
    const user = userEvent.setup();
    setControllerData({});
    let resolveRestart: () => void = () => {};
    vi.mocked(restartController).mockImplementation(
      () => new Promise<void>((resolve) => { resolveRestart = resolve; }),
    );
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^restart rolls/i }));
    await user.click(screen.getByRole("button", { name: /^yes, restart$/i }));

    // Still showing the confirm dialog, now in-flight: disabled with a progress label
    // instead of closing immediately with no feedback.
    const pendingButton = await screen.findByRole("button", { name: /^restarting…$/i });
    expect(pendingButton).toBeDisabled();
    expect(screen.getByRole("button", { name: /^cancel$/i })).toBeDisabled();

    resolveRestart();
    await waitFor(() => expect(screen.queryByText(/Yes, restart/)).not.toBeInTheDocument());
  });
});

describe("ControllerDetail — Reconciliation mode toggle", () => {
  beforeEach(() => {
    openConfiguration = true;
    vi.clearAllMocks();
    setBundles([]);
  });

  it("shows reconciliation mode and interval in Configuration", () => {
    setControllerData({
      reconciliationPolicy: { mode: "automatic", interval: "30s" },
    });
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText("automatic")).toBeInTheDocument();
  });

  it("shows Edit policy button for reconciliation policy", () => {
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText("Edit policy")).toBeInTheDocument();
  });

  it("shows policy edit form when Edit is clicked", async () => {
    const user = userEvent.setup();
    setControllerData({
      reconciliationPolicy: { mode: "automatic", interval: "30s" },
    });
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByText("Edit policy"));

    expect(screen.getByText("Save Policy")).toBeInTheDocument();
    // Should show mode selector
    expect(screen.getByDisplayValue("Automatic — push on config drift")).toBeInTheDocument();
  });

  // Clearing interval sends interval: null — an explicit removal another
  // manager may retain. A response carrying unappliedRemovals must surface a
  // non-blocking notice instead of an unqualified success.
  it("surfaces a non-blocking notice when a cleared policy field is retained", async () => {
    setControllerData({
      reconciliationPolicy: { mode: "automatic", interval: "30s" },
    });
    mockUpdateController.mockResolvedValue({
      unappliedRemovals: [{ field: "spec.reconciliationPolicy.interval" }],
    });
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByText("Edit policy"));
    // Clear the interval — the patch then sends interval: null.
    await user.clear(screen.getByPlaceholderText("30s (default)"));
    await user.click(screen.getByText("Save Policy"));

    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith(
        expect.stringContaining("spec.reconciliationPolicy.interval could not be removed"),
      ),
    );
    expect(mockToast).not.toHaveBeenCalledWith("Reconciliation policy updated");
  });

  it("reports an unqualified success when no removal was blocked", async () => {
    setControllerData({
      reconciliationPolicy: { mode: "automatic", interval: "30s" },
    });
    mockUpdateController.mockResolvedValue({});
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByText("Edit policy"));
    await user.clear(screen.getByPlaceholderText("30s (default)"));
    await user.click(screen.getByText("Save Policy"));

    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith("Reconciliation policy updated"),
    );
    expect(mockToast).not.toHaveBeenCalledWith(expect.stringMatching(/could not be removed/));
  });
});

describe("ControllerDetail — Routing-mode links", () => {
  beforeEach(() => {
    setBundles([]);
  });

  it("shows View embedded only for path-mode controllers", () => {
    setControllerData({ routingMode: "path" } as Partial<typeof mockControllerData>);
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText(/View embedded/)).toBeInTheDocument();
    expect(screen.getByText(/Open Jenkins/)).toBeInTheDocument();
  });

  it("hides the embedded link for subdomain controllers but keeps Open Jenkins", () => {
    setControllerData({ routingMode: "subdomain" } as Partial<typeof mockControllerData>);
    renderWithProviders(<ControllerDetail />);

    expect(screen.queryByText(/View embedded/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Managed view/)).not.toBeInTheDocument();
    expect(screen.getByText(/Open Jenkins/)).toBeInTheDocument();
  });
});

describe("ControllerDetail — Health probes", () => {
  beforeEach(() => {
    openConfiguration = true;
    vi.clearAllMocks();
    setBundles([]);
    setControllerData({
      probes: {
        startup: { failureThreshold: 60 },
        liveness: { disabled: true },
      },
    });
    mockCanDo.mockImplementation(() => true);
  });

  it("renders probes for editors, exercises each toggle, and saves the updated probes spec", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText("Health probes")).toBeInTheDocument();
    expect(screen.getByLabelText(/startup failure threshold/i)).toHaveValue(60);
    expect(screen.getByLabelText(/enable liveness probe/i)).not.toBeChecked();
    expect(screen.getByLabelText(/liveness initial delay seconds/i)).toBeDisabled();

    await user.clear(screen.getByLabelText(/startup initial delay seconds/i));
    await user.type(screen.getByLabelText(/startup initial delay seconds/i), "21");
    await user.clear(screen.getByLabelText(/startup period seconds/i));
    await user.type(screen.getByLabelText(/startup period seconds/i), "22");
    await user.clear(screen.getByLabelText(/startup timeout seconds/i));
    await user.type(screen.getByLabelText(/startup timeout seconds/i), "23");
    await user.clear(screen.getByLabelText(/startup failure threshold/i));
    await user.type(screen.getByLabelText(/startup failure threshold/i), "24");
    await user.clear(screen.getByLabelText(/startup success threshold/i));
    await user.type(screen.getByLabelText(/startup success threshold/i), "25");

    const readinessEnabled = screen.getByLabelText(/enable readiness probe/i);
    const livenessEnabled = screen.getByLabelText(/enable liveness probe/i);

    await user.click(screen.getByLabelText(/enable startup probe/i));
    expect(screen.getByLabelText(/startup initial delay seconds/i)).toBeDisabled();
    await user.click(screen.getByLabelText(/enable startup probe/i));
    expect(screen.getByLabelText(/startup initial delay seconds/i)).toBeEnabled();

    await user.click(readinessEnabled);
    expect(screen.getByLabelText(/readiness initial delay seconds/i)).toBeDisabled();
    await user.click(readinessEnabled);
    expect(screen.getByLabelText(/readiness initial delay seconds/i)).toBeEnabled();

    await user.click(livenessEnabled);
    expect(screen.getByLabelText(/liveness initial delay seconds/i)).toBeEnabled();
    await user.click(livenessEnabled);
    expect(screen.getByLabelText(/liveness initial delay seconds/i)).toBeDisabled();

    await user.click(screen.getByRole("button", { name: /save probes/i }));

    await waitFor(() => {
      expect(mockUpdateController).toHaveBeenCalledWith(
        "core",
        "my-ctrl",
        "my-ns",
        {
          spec: {
            probes: {
              startup: {
                initialDelaySeconds: 21,
                periodSeconds: 22,
                timeoutSeconds: 23,
                failureThreshold: 24,
                successThreshold: 25,
              },
              liveness: { disabled: true },
            },
          },
        },
        { force: false },
      );
    });
  });
});

describe("ControllerDetail — Spec Editor card", () => {
  beforeEach(() => {
    openConfiguration = true;
    vi.clearAllMocks();
    setBundles([]);
    setControllerData({});
    mockCanDo.mockImplementation(() => true);
  });

  it("renders the Spec Editor card for an admin", () => {
    setControllerData({});
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Spec Editor/)).toBeInTheDocument();
    expect(screen.getByText(/Form/)).toBeInTheDocument();
  });

  it("renders spec editor tabs", () => {
    setControllerData({});
    renderWithProviders(<ControllerDetail />);
    expect(screen.getByText(/Pod overrides/)).toBeInTheDocument();
    expect(screen.getByText(/Resource overlay/)).toBeInTheDocument();
    expect(screen.getByText(/Save spec/)).toBeInTheDocument();
  });
});

describe("ControllerDetail — Hibernation settings", () => {
  beforeEach(() => {
    openConfiguration = true;
    vi.clearAllMocks();
    setBundles([]);
    mockCanDo.mockImplementation(() => true);
  });

  // HibernationCard must seed its state from the projected spec (spec.hibernation),
  // not from a non-existent top-level DTO field. Before the fix it always read
  // undefined and rendered the defaults regardless of the stored configuration.
  it("seeds the hibernation card from spec.hibernation, not the defaults", () => {
    setControllerData({
      spec: {
        hibernation: {
          enabled: true,
          gracePeriodMinutes: 15,
          activityIgnoreRegex: "^nightly-",
        },
      },
    });
    renderWithProviders(<ControllerDetail />);

    const card = screen
      .getByText("Hibernation")
      .closest("div")!
      .parentElement!.parentElement! as HTMLElement;

    // Read-only stats reflect the stored values, not the defaults.
    expect(within(card).getByText("Yes")).toBeInTheDocument();
    expect(within(card).getByText("15")).toBeInTheDocument();
    expect(within(card).getByText("^nightly-")).toBeInTheDocument();
    expect(within(card).queryByText("(none)")).not.toBeInTheDocument();

    // Editable controls are seeded from the same values.
    expect(within(card).getByRole("checkbox")).toBeChecked();
    expect(within(card).getByRole("spinbutton")).toHaveValue(15);
    expect(within(card).getByPlaceholderText("/path/to/ignore.*")).toHaveValue("^nightly-");
  });

  it("falls back to defaults when spec.hibernation is absent", () => {
    setControllerData({ spec: {} });
    renderWithProviders(<ControllerDetail />);

    const card = screen
      .getByText("Hibernation")
      .closest("div")!
      .parentElement!.parentElement! as HTMLElement;

    expect(within(card).getByText("No")).toBeInTheDocument();
    expect(within(card).getByText("60")).toBeInTheDocument();
    expect(within(card).getByText("(none)")).toBeInTheDocument();
    expect(within(card).getByRole("checkbox")).not.toBeChecked();
    expect(within(card).getByRole("spinbutton")).toHaveValue(60);
  });

  // Renders the full provider tree so rerender keeps QueryClientProvider in
  // place (this file's renderWithProviders nests the providers inline).
  function rerenderCard(rerender: (ui: React.ReactElement) => void) {
    rerender(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <ControllerDetail />
        </MemoryRouter>
      </QueryClientProvider>
    );
  }

  function getCard() {
    return screen
      .getByText("Hibernation")
      .closest("div")!
      .parentElement!.parentElement! as HTMLElement;
  }

  // The card must track ctrl PAST first render: a pristine background refetch
  // (another writer changed the value on the server) must update what the card
  // shows. A plain useState(initialProp) reads its argument once and would keep
  // showing the original value forever — the hydration effect is what re-syncs.
  it("re-syncs from a refetch after first render (pristine)", () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    const { rerender } = renderWithProviders(<ControllerDetail />);

    expect(within(getCard()).getByRole("spinbutton")).toHaveValue(15);

    // Another writer bumps gracePeriodMinutes; the refetch updates ctrl.
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 30, activityIgnoreRegex: "^nightly-" },
      },
    });
    rerenderCard(rerender);

    expect(within(getCard()).getByRole("spinbutton")).toHaveValue(30);
  });

  // In-progress edits must survive a background refetch: the user's unsaved
  // value wins over the server's newer value (mirroring SpecEditorCard's dirty
  // check — a dirty card is not re-hydrated until a save succeeds).
  it("does not clobber in-progress edits on a refetch", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    const { rerender } = renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    const spinbutton = within(getCard()).getByRole("spinbutton");
    await user.clear(spinbutton);
    await user.type(spinbutton, "20");
    expect(within(getCard()).getByRole("spinbutton")).toHaveValue(20);

    // Another writer changes the server value while the user has an unsaved edit.
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 30, activityIgnoreRegex: "^nightly-" },
      },
    });
    rerenderCard(rerender);

    // The user's edit is preserved, not overwritten by the refetch.
    expect(within(getCard()).getByRole("spinbutton")).toHaveValue(20);
  });

  // A successful save rebases from the RESPONSE: the card reflects the saved
  // values, stops being dirty, and follows subsequent refetches again.
  it("rebases from the save response and clears the dirty state", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    mockUpdateController.mockResolvedValue({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 45, activityIgnoreRegex: "^nightly-" },
      },
    });
    const { rerender } = renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    const spinbutton = within(getCard()).getByRole("spinbutton");
    await user.clear(spinbutton);
    await user.type(spinbutton, "20");

    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));
    await waitFor(() => expect(mockUpdateController).toHaveBeenCalled());

    // Card shows the response's saved value, not the stale local edit.
    expect(within(getCard()).getByRole("spinbutton")).toHaveValue(45);

    // And it is no longer dirty: a refetch re-syncs to the server again.
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 50, activityIgnoreRegex: "^nightly-" },
      },
    });
    rerenderCard(rerender);
    expect(within(getCard()).getByRole("spinbutton")).toHaveValue(50);
  });

  // An edit made while a save is in flight must keep the card out of the
  // post-save rebase (mirrors SpecEditorCard's "only tiers whose version is
  // unchanged since the save started are rebased" rule): the newer edit wins
  // over the save response.
  it("keeps edits made while a save is in flight out of the rebase", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    let resolveUpdate!: (v: unknown) => void;
    mockUpdateController.mockReturnValue(
      new Promise((res) => {
        resolveUpdate = res;
      })
    );
    renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    const spinbutton = () => within(getCard()).getByRole("spinbutton");
    await user.clear(spinbutton());
    await user.type(spinbutton(), "20");

    // Start a save that hangs until we resolve the mock.
    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));
    expect(mockUpdateController).toHaveBeenCalled();

    // While the save is in flight the user keeps editing.
    await user.clear(spinbutton());
    await user.type(spinbutton(), "25");

    // The save resolves with a value older than the user's latest edit.
    await act(async () => {
      resolveUpdate({
        spec: {
          hibernation: { enabled: true, gracePeriodMinutes: 45, activityIgnoreRegex: "^nightly-" },
        },
      });
    });

    // The user's newer edit is preserved, not rebased to 45.
    expect(spinbutton()).toHaveValue(25);
  });

  // Clearing activityIgnoreRegex sends an explicit null removal that another
  // manager may retain. A response carrying unappliedRemovals must surface a
  // non-blocking notice instead of an unqualified success.
  it("surfaces a non-blocking notice when clearing activityIgnoreRegex is retained", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    mockUpdateController.mockResolvedValue({
      unappliedRemovals: [{ field: "spec.hibernation.activityIgnoreRegex" }],
    });
    renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    // Clear the regex — the save patch then sends activityIgnoreRegex: null.
    await user.clear(screen.getByPlaceholderText("/path/to/ignore.*"));
    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));

    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith(
        expect.stringContaining("spec.hibernation.activityIgnoreRegex could not be removed"),
      ),
    );
    expect(mockToast).not.toHaveBeenCalledWith("Hibernation settings updated");
  });

  it("reports an unqualified success when no removal was blocked", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    mockUpdateController.mockResolvedValue({});
    renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    await user.clear(screen.getByPlaceholderText("/path/to/ignore.*"));
    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));

    await waitFor(() =>
      expect(mockToast).toHaveBeenCalledWith("Hibernation settings updated"),
    );
    expect(mockToast).not.toHaveBeenCalledWith(expect.stringMatching(/could not be removed/));
  });

  // The save patch must be a DIFF against the hydration baseline, not the whole
  // object: hydrating all three fields and changing ONLY enabled emits a
  // request whose spec.hibernation carries exactly { enabled }. A stale
  // gracePeriodMinutes/activityIgnoreRegex (changed by another writer since the
  // card hydrated) must not be re-asserted — re-sending it would silently
  // revert the other writer's change.
  it("sends only the changed field, diffing against the hydrated baseline", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    mockUpdateController.mockResolvedValue({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    // Change ONLY enabled.
    await user.click(within(getCard()).getByRole("checkbox"));
    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalled());
    const patch = mockUpdateController.mock.calls[0][3] as Record<string, any>;
    expect(patch).toEqual({ spec: { hibernation: { enabled: false } } });
  });

  // An untouched empty activityIgnoreRegex must NOT become a null removal
  // request on every save: emitting activityIgnoreRegex: null for a value that
  // was already empty turns every save into a removal request that another
  // manager may retain — surfacing a spurious "could not be removed" notice.
  it("does not emit a removal request for an untouched empty activityIgnoreRegex", async () => {
    setControllerData({
      spec: { hibernation: { enabled: true, gracePeriodMinutes: 15 } },
    });
    mockUpdateController.mockResolvedValue({
      spec: { hibernation: { enabled: true, gracePeriodMinutes: 15 } },
    });
    renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    // Change ONLY enabled; the regex was never set and stays untouched.
    await user.click(within(getCard()).getByRole("checkbox"));
    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalled());
    const patch = mockUpdateController.mock.calls[0][3] as Record<string, any>;
    expect(patch).toEqual({ spec: { hibernation: { enabled: false } } });
    // No activityIgnoreRegex key at all — not even a null.
    expect(patch.spec.hibernation).not.toHaveProperty("activityIgnoreRegex");
  });

  // A stored regex with surrounding whitespace must be treated as UNTOUCHED
  // when the user never edits it. The diff compares the raw value against the
  // raw baseline (round-7 regression: a trimmed value compared against an
  // untrimmed baseline made every save re-emit an untouched whitespace-padded
  // regex — silently rewriting a field the user never touched, changing the
  // spec, and rolling the pod).
  it("does not re-send an untouched activityIgnoreRegex that has surrounding whitespace", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: " ^/health$" },
      },
    });
    mockUpdateController.mockResolvedValue({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: " ^/health$" },
      },
    });
    renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    // The regex is hydrated verbatim, leading whitespace preserved.
    expect(within(getCard()).getByPlaceholderText("/path/to/ignore.*")).toHaveValue(" ^/health$");

    // Change ONLY enabled.
    await user.click(within(getCard()).getByRole("checkbox"));
    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));

    await waitFor(() => expect(mockUpdateController).toHaveBeenCalled());
    const patch = mockUpdateController.mock.calls[0][3] as Record<string, any>;
    expect(patch).toEqual({ spec: { hibernation: { enabled: false } } });
    // No activityIgnoreRegex key at all — the untouched regex must not be re-sent.
    expect(patch.spec.hibernation).not.toHaveProperty("activityIgnoreRegex");
  });

  // A save with no differences issues no request at all (8.4).
  it("issues no request when nothing changed", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));
    expect(mockUpdateController).not.toHaveBeenCalled();
  });

  // An edit that reverts to the hydrated baseline leaves nothing to save, and
  // must also clear the dirty flag: the card is genuinely pristine, so the
  // hydration effect must keep re-syncing on background refetches. Otherwise
  // the card stays "dirty" forever and silently goes stale.
  it("clears the dirty flag when edits revert to the hydrated baseline", async () => {
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 15, activityIgnoreRegex: "^nightly-" },
      },
    });
    const { rerender } = renderWithProviders(<ControllerDetail />);
    const user = userEvent.setup();

    // Edit gracePeriodMinutes away from and back to the hydrated value.
    const spinbutton = within(getCard()).getByRole("spinbutton");
    await user.clear(spinbutton);
    await user.type(spinbutton, "20");
    await user.clear(spinbutton);
    await user.type(spinbutton, "15");

    await user.click(screen.getByRole("button", { name: /Save hibernation settings/ }));
    expect(mockUpdateController).not.toHaveBeenCalled();

    // The card is no longer dirty: a background refetch (another writer sets
    // 30) must re-sync the card from the server.
    setControllerData({
      spec: {
        hibernation: { enabled: true, gracePeriodMinutes: 30, activityIgnoreRegex: "^nightly-" },
      },
    });
    rerenderCard(rerender);
    expect(within(getCard()).getByRole("spinbutton")).toHaveValue(30);
  });
});

// ---- New tests per review requirements ----

describe("ControllerDetail — SSE logs lifecycle", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setControllerData({});
    mockCanDo.mockImplementation(() => true);
  });

  it("shows Logs card rendered under Diagnostics", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByText(/Diagnostics/));

    // Logs card renders with filter buttons
    expect(screen.getAllByText(/All/).length).toBeGreaterThanOrEqual(1);
  });

  it("renders Logs always mounted under Diagnostics", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByText(/Diagnostics/));
    // Logs and Activity render directly without sub-tabs
    expect(screen.getAllByText(/All/).length).toBeGreaterThanOrEqual(1);
  });

  it("renders with level filter buttons in Logs card", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByText(/Diagnostics/));

    // Logs card renders with level buttons
    expect(screen.getAllByText(/All|Operator|Mite|Jenkins/).length).toBeGreaterThanOrEqual(4);
  });

  it("renders an inline reconnecting row when the log stream errors", async () => {
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
      // restore the default so later tests in this file see a closed/no-error stream
      vi.mocked(useEventStream).mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
    }
  });
});

describe("ControllerDetail — Permission matrix", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setControllerData({});
    mockCanDo.mockImplementation(() => true);
    openConfiguration = false;
  });

  it("shows Reprovision with manage permission in header", async () => {
    const user = userEvent.setup();
    setControllerData({});
    renderWithProviders(<ControllerDetail />);
    // reprovision in header menu, gated by controllers:manage
    await user.click(screen.getByRole("button", { name: /more actions/i }));
    expect(screen.getByRole("menuitem", { name: /reprovision/i })).toBeInTheDocument();
  });

  it("shows restart confirmation with approve-restart permission", async () => {
    const user = userEvent.setup();
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /^restart rolls/i }));
    expect(screen.getByText(/Yes, restart/)).toBeInTheDocument();
  });

  it("shows Reload Configuration button when pendingRestart exists (approve-restart)", () => {
    setControllerData({ pendingRestart: { detectedAt: new Date().toISOString(), desiredStateHash: "abc", changes: ["jcasc"] } } as any);
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText(/Reload Configuration/)).toBeInTheDocument();
  });

  it("hides Reload Configuration when approve-restart is denied", () => {
    mockCanDo.mockImplementation(
      (_perms?: unknown, _ns?: string, resource?: string, verb?: string) =>
        !(resource === "controllers" && verb === "approve-restart")
    );
    setControllerData({ pendingRestart: { detectedAt: new Date().toISOString(), desiredStateHash: "abc", changes: ["jcasc"] } } as any);
    renderWithProviders(<ControllerDetail />);

    expect(screen.queryByText(/Reload Configuration/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Safe Restart/)).not.toBeInTheDocument();
  });

  it("hides Approve deletion buttons when approve-deletion is denied", () => {
    mockCanDo.mockImplementation(
      (_perms?: unknown, _ns?: string, resource?: string, verb?: string) =>
        !(resource === "controllers" && verb === "approve-deletion")
    );
    setControllerData({ pendingItemDeletions: [{ path: "job/test", reason: "build running", detectedAt: new Date().toISOString() }] } as any);
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText(/Pending item deletion/)).toBeInTheDocument();
    expect(screen.queryByText(/Approve deletion/)).not.toBeInTheDocument();
  });

  it("hides header lifecycle buttons when manage is denied", () => {
    mockCanDo.mockImplementation(
      (_perms?: unknown, _ns?: string, resource?: string, verb?: string) =>
        !(resource === "controllers" && verb === "manage")
    );
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    expect(screen.queryByText(/Reprovision/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Power Off/)).not.toBeInTheDocument();
    expect(screen.queryByText(/↻ Restart/)).not.toBeInTheDocument();
  });

  it("does not render Spec Editor without controllers:update", () => {
    openConfiguration = true;
    mockCanDo.mockImplementation(
      (_perms?: unknown, _ns?: string, resource?: string, verb?: string) =>
        !(resource === "controllers" && verb === "update")
    );
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    // Spec Editor still renders (Card wrapper), but Save button is hidden
    expect(screen.getByText(/Spec Editor/)).toBeInTheDocument();
  });

  it("renders edit controls with controllers:update permission", () => {
    openConfiguration = true;
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    expect(screen.getByText(/Edit policy/)).toBeInTheDocument();
  });

  it("viewer without update sees read-only Overview with Spec and Bundle summary", () => {
    mockCanDo.mockImplementation(() => false);
    setControllerData({});
    renderWithProviders(<ControllerDetail />);

    // Overview shows read-only data
    expect(screen.getByText("Spec")).toBeInTheDocument();
    // No header mutation buttons
    expect(screen.queryByText(/\u27F3 Reprovision/)).not.toBeInTheDocument();
    expect(screen.queryByText(/\u21BB Restart/)).not.toBeInTheDocument();
  });
});

describe("ControllerDetail — Lazy mount and single detail query", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setControllerData({});
    mockCanDo.mockImplementation(() => true);
    openConfiguration = false;
  });

  it("only mounts Overview tab by default (lazy tabs)", () => {
    renderWithProviders(<ControllerDetail />);

    expect(screen.queryByTestId("configuration-tab")).not.toBeInTheDocument();
    expect(screen.queryByTestId("observability-tab")).not.toBeInTheDocument();
    expect(screen.queryByTestId("diagnostics-tab")).not.toBeInTheDocument();
  });

  it("mounts a tab only when selected", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    expect(screen.queryByTestId("configuration-tab")).not.toBeInTheDocument();

    await user.click(screen.getAllByText(/Configuration/)[0]);
    expect(screen.getByTestId("configuration-tab")).toBeInTheDocument();
  });

  it("does not duplicate detail query and switches all tabs", async () => {
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    // Switch to each tab and verify content renders
    await user.click(screen.getAllByText(/Configuration/)[0]);
    expect(screen.getByTestId("configuration-tab")).toBeInTheDocument();

    await user.click(screen.getByText(/Diagnostics/));
    expect(screen.getByTestId("diagnostics-tab")).toBeInTheDocument();

    // Each tab shares the same page-owned controller data without duplicate requests
    expect(screen.getByText(/my-ctrl/)).toBeInTheDocument();
  });
});

describe("ControllerDetail — Mutation refresh triggers invalidation", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setBundles([]);
    setControllerData({});
    mockCanDo.mockImplementation(() => true);
    openConfiguration = false;
  });

  it("reprovision in header triggers detail+list invalidation", async () => {
    const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");
    const user = userEvent.setup();
    renderWithProviders(<ControllerDetail />);

    await user.click(screen.getByRole("button", { name: /more actions/i }));
    await user.click(screen.getByRole("menuitem", { name: /reprovision/i }));
    await user.click(screen.getByRole("button", { name: /yes, reprovision/i }));

    await waitFor(() => {
      expect(invalidateSpy).toHaveBeenCalledWith(
        expect.objectContaining({ queryKey: ["controller", "core", "my-ns", "my-ctrl"] })
      );
      expect(invalidateSpy).toHaveBeenCalledWith(
        expect.objectContaining({ queryKey: ["controllers"] })
      );
    });
  });
});
