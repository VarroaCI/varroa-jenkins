import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import ControllerWizard from "./ControllerWizard";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", () => ({
  useNavigate: () => mockNavigate,
}));

const mockGetProvisioningConfig = vi.fn();
const mockGetDeployableNamespaces = vi.fn();
const mockGetController = vi.fn();
const mockListComposedBundles = vi.fn();
const mockListCatalogItems = vi.fn();
const mockListGroups = vi.fn();
const mockPreviewComposedBundle = vi.fn();
const mockPreviewControllerOverlay = vi.fn();
const mockPreflightController = vi.fn();
const mockRenderController = vi.fn();
const mockCreateController = vi.fn();

const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  ApiError: class ApiError extends Error {
    constructor(public readonly status: number, message: string, public readonly body?: unknown) {
      super(message);
      this.name = "ApiError";
    }
  },
}));

const mockUseClustersData = { data: [{name:"core",core:true,healthy:true,lastHeartbeat:"2025-01-01T00:00:00Z",operatorVersion:"1.0",k8sVersion:"1.28",controllerCount:5,connectedCount:4,state:"active"}], isLoading: false, isError: false };
vi.mock("../hooks/useClusters", () => ({ useClusters: () => mockUseClustersData, coreOf: (c: unknown[]) => c?.find((c2: any) => c2.core) }));

vi.mock("../api/client", () => ({
  listClusters: vi.fn(),
  getProvisioningConfig: () => mockGetProvisioningConfig(),
  getDeployableNamespaces: (cluster: string) => mockGetDeployableNamespaces(cluster),
  getController: (...args: unknown[]) => mockGetController(...args),
  listComposedBundles: () => mockListComposedBundles(),
  listCatalogItems: () => mockListCatalogItems(),
  listGroups: () => mockListGroups(),
  previewComposedBundle: (...args: unknown[]) => mockPreviewComposedBundle(...args),
  previewControllerOverlay: (...args: unknown[]) => mockPreviewControllerOverlay(...args),
  preflightController: (...args: unknown[]) => mockPreflightController(...args),
  renderController: (...args: unknown[]) => mockRenderController(...args),
  createController: (...args: unknown[]) => mockCreateController(...args),
  controllerEventsUrl: (_ns: string, _name: string) => "/api/v1/controllers/test-ns/test-ctrl/events",
}));

const DEFAULT_CONFIG = {
  rootDomain: "jenkins.example.com",
  dashboardHost: "varroa.example.com",
  defaultNamespace: "varroa",
  namespaces: ["varroa", "varroa-tenants"],
  defaultVersion: "2.479.3",
  versions: [
    { version: "2.479.3", channel: "lts", recommended: true, eol: "" },
    { version: "2.462.3", channel: "lts", recommended: false, eol: "2026-10-01" },
  ],
  sizePresets: [
    { name: "S", cpu: "1", memory: "2Gi", storage: "10Gi" },
    { name: "M", cpu: "2", memory: "4Gi", storage: "20Gi" },
  ],
  injectedVariables: ["varroa_controller_name"],
};

beforeEach(() => {
  mockNavigate.mockReset();
  mockBffFetch.mockResolvedValue({ ticket: "tkn", expiresInSeconds: 120 });
  // Clear accumulated call history so tests asserting mock.calls[0] see only
  // their own invocation (multiple tests drive createController/preview).
  mockCreateController.mockClear();
  mockBffFetch.mockClear();
  mockPreviewComposedBundle.mockClear();
  mockListComposedBundles.mockClear();
  mockGetProvisioningConfig.mockResolvedValue(DEFAULT_CONFIG);
  mockGetDeployableNamespaces.mockResolvedValue({
    namespaces: ["varroa", "varroa-tenants"],
    defaultNamespace: "varroa",
    allowFreeform: false,
    degraded: false,
  });
  mockGetController.mockRejectedValue(new Error("not found"));
  mockListComposedBundles.mockResolvedValue({ items: [] });
  mockListCatalogItems.mockResolvedValue({ items: [] });
  mockListGroups.mockResolvedValue([]);
  mockPreviewComposedBundle.mockResolvedValue({
    jenkinsYaml: "jenkins:\n  url: http://example.com",
    pluginsYaml: "",
    itemsYaml: "",
    rbacYaml: "",
    missing: [],
    drifted: [],
    warnings: [],
    unresolvedVariables: [],
  });
  mockPreflightController.mockResolvedValue({ checks: [] });
  mockRenderController.mockResolvedValue("apiVersion: varroa.dev/v1alpha1\nkind: Controller\n");
  mockCreateController.mockResolvedValue({});
});

describe("ControllerWizard", () => {
  it("renders the wizard shell with stepper after config loads", async () => {
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.getByText("New controller")).toBeInTheDocument();
    });
    expect(screen.getAllByText("Basics").length).toBeGreaterThan(0);
    expect(screen.getByText("Configuration bundle")).toBeInTheDocument();
  });

  it("does not reserve the rail column on steps without rail content", async () => {
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.getByText("New controller")).toBeInTheDocument();
    });

    // The rail only carries the Timeline card on the review step. Rendering it
    // as an empty div on steps 1-4 still reserved its ~490px grid column, which
    // pushed the form card short of the stepper's right edge.
    expect(screen.queryByText("Timeline")).not.toBeInTheDocument();

    const wizard = document.querySelector('[class*="wizard"]')!;
    expect(wizard).toBeTruthy();
    // Single-column on step 1, and no empty grid child.
    expect(wizard.className).toMatch(/wizardSolo/);
    expect(wizard.className).not.toMatch(/wizardWithRail/);
    expect(wizard.children.length).toBe(1);
  });

  it("fills step 1 and navigates to step 2", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    expect(screen.getByText(/ComposedBundle/i)).toBeInTheDocument();
  });

  it("can go back from step 2", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByRole("button", { name: /← Back$/ }));
    expect(screen.getByPlaceholderText("team-web-builds")).toBeInTheDocument();
  });

  it("navigates through all 5 steps", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });

    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));

    await waitFor(() => {
      expect(screen.getAllByText("Advanced options").length).toBeGreaterThan(0);
    });
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));

    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });
    expect(screen.getByRole("button", { name: /deploy controller|deployed/i })).toBeInTheDocument();
  });

  it("back link navigates to /controllers", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.click(screen.getByText(/back to controllers/i));
    expect(mockNavigate).toHaveBeenCalledWith("/controllers");
  });

  it("shows namespace select from config", async () => {
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    const select = screen.getByRole("combobox");
    expect(select).toBeInTheDocument();
    expect((select as HTMLSelectElement).value).toBe("varroa");
  });

  it("shows version cards from config", async () => {
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    expect(screen.getByText("2.479.3")).toBeInTheDocument();
  });

  it("calls preflight on step 5 entry", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));

    await waitFor(() => {
      expect(mockPreflightController).toHaveBeenCalled();
    });
  });

  it("downloads YAML", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));

    await waitFor(() => {
      expect(mockPreflightController).toHaveBeenCalled();
    });

    const dlBtn = screen.getByRole("button", { name: /download yaml/i });
    await user.click(dlBtn);
    await waitFor(() => {
      expect(mockRenderController).toHaveBeenCalled();
    });
  });

  it("validates cpu input rejects invalid values", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));

    const cpuInput = screen.getByPlaceholderText("2") as HTMLInputElement;
    await user.clear(cpuInput);
    await user.type(cpuInput, "abc");

    const next = screen.getByRole("button", { name: /continue.*advanced options/i });
    expect(next).toBeDisabled();
  });

  it("blocks continue past advanced options with unparsable podOverrides YAML", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Advanced options").length).toBeGreaterThan(0);
    });

    fireEvent.change(screen.getByLabelText("podOverrides YAML"), { target: { value: "not: valid: yaml: [" } });

    const next = screen.getByRole("button", { name: /continue.*review/i });
    expect(next).toBeDisabled();
    expect(screen.getByText(/invalid yaml/i)).toBeInTheDocument();
  });

  it("blocks continue past advanced options with a non-numeric rolloutWave", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Advanced options").length).toBeGreaterThan(0);
    });

    await user.type(screen.getByPlaceholderText("rolloutWave"), "abc");

    const next = screen.getByRole("button", { name: /continue.*review/i });
    expect(next).toBeDisabled();
    expect(screen.getByText(/must be whole numbers/i)).toBeInTheDocument();
  });

  it("renders missing/drifted banners from preview and still shows the YAML", async () => {
    mockPreviewComposedBundle.mockResolvedValue({
      jenkinsYaml: "jenkins:\n  url: http://example.com",
      pluginsYaml: "",
      itemsYaml: "",
      rbacYaml: "",
      missing: ["ghost-item"],
      drifted: ["old-item"],
      warnings: [],
      unresolvedVariables: [],
    });
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("Compose new"));
    await user.click(screen.getByRole("button", { name: /preview merged jcasc/i }));

    await waitFor(() => {
      expect(screen.getByText(/Missing items:.*ghost-item/)).toBeInTheDocument();
    });
    expect(screen.getByText(/Drifted items:.*old-item/)).toBeInTheDocument();
    // The composed YAML still renders below the banners.
    expect(screen.getByText(/jenkins:/)).toBeInTheDocument();
  });

  it("renders no missing/drifted banners when preview returns empty arrays", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("Compose new"));
    await user.click(screen.getByRole("button", { name: /preview merged jcasc/i }));

    await waitFor(() => {
      expect(screen.getByText(/jenkins:/)).toBeInTheDocument();
    });
    expect(screen.queryByText(/Missing items:/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Drifted items:/)).not.toBeInTheDocument();
  });

  it("emits composedBundleRef with the selected bundle's namespace", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    mockListComposedBundles.mockResolvedValue({
      items: [
        {
          apiVersion: "varroa.dev/v1alpha1",
          kind: "ComposedBundle",
          metadata: { name: "platform-bundle", namespace: "varroa-system" },
          spec: { displayName: "Platform Bundle", inputs: [] },
          status: { phase: "Ready", resolvedHash: "abcdef1234567" },
        },
      ],
    });

    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));

    // Existing mode is the default; select the bundle from another namespace.
    await waitFor(() => {
      expect(screen.getByText("Platform Bundle")).toBeInTheDocument();
    });
    await user.click(screen.getByText("Platform Bundle"));

    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });

    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });
    const [, , body] = mockCreateController.mock.calls[0];
    expect(body.spec.composedBundleRef).toEqual({ name: "platform-bundle", namespace: "varroa-system" });

    vi.unstubAllGlobals();
  });

  // Regression for #528: the BFF requires path-mode ingress to use the
  // dashboard host, but the wizard never supplied it — a blank host submitted
  // no host at all and the create call 400ed after an all-green preflight.
  it("path mode with a blank host submits the dashboard host", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");

    await user.click(screen.getByText("Path-based"));
    // The hint names the dashboard host from provisioning config.
    expect(screen.getAllByText("varroa.example.com").length).toBeGreaterThan(0);

    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });

    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });
    const [, , pathBody] = mockCreateController.mock.calls[0];
    expect(pathBody.spec.ingressSpec).toMatchObject({ mode: "path", host: "varroa.example.com" });

    vi.unstubAllGlobals();
  });

  it("fills every advanced option and deploys", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Advanced options").length).toBeGreaterThan(0);
    });

    await user.type(screen.getByPlaceholderText("cluster default"), "traefik");

    await user.type(screen.getByPlaceholderText("annotation key"), "nginx.ingress.kubernetes.io/rewrite-target");
    await user.type(screen.getByPlaceholderText("value"), "/");
    await user.click(screen.getByRole("button", { name: "+ Add" }));
    expect(screen.getByText("nginx.ingress.kubernetes.io/rewrite-target")).toBeInTheDocument();
    await user.click(screen.getByTitle("Remove"));
    expect(screen.queryByText("nginx.ingress.kubernetes.io/rewrite-target")).not.toBeInTheDocument();

    await user.selectOptions(screen.getByDisplayValue("Running (default)"), "Stopped");
    // Hibernated is not a power state — the wizard must not offer it.
    expect(screen.queryByRole("option", { name: "Hibernated" })).not.toBeInTheDocument();
    expect(screen.queryByRole("option", { name: "Stopped" })).toBeInTheDocument();
    await user.selectOptions(screen.getByDisplayValue("Mode: cluster default"), "automatic");
    await user.type(screen.getByPlaceholderText("interval, e.g. 30s"), "30s");
    await user.type(screen.getByPlaceholderText("rolloutWave"), "1");
    await user.type(screen.getByPlaceholderText("maxDeferSeconds"), "60");
    await user.type(screen.getByPlaceholderText("drainTimeoutSeconds"), "120");

    await user.type(screen.getByPlaceholderText("class name"), "prod-large");
    await user.type(screen.getByPlaceholderText("cpu"), "100m");
    await user.type(screen.getByPlaceholderText("memory"), "128Mi");

    await user.click(screen.getByRole("checkbox", { name: /backup/i }));
    await user.type(screen.getByPlaceholderText("schedule, e.g. 0 2 * * *"), "0 2 * * *");
    await user.type(screen.getByPlaceholderText("retentionDays"), "7");

    fireEvent.change(screen.getByLabelText("StatefulSet overlay YAML"), { target: { value: "spec: foo" } });

    await user.click(screen.getByRole("button", { name: /preview merge/i }));
    await waitFor(() => {
      expect(mockPreviewControllerOverlay).toHaveBeenCalled();
    });

    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });

    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });
    const [, , body] = mockCreateController.mock.calls[0];
    expect(body.spec.ingressSpec.ingressClassName).toBe("traefik");
    expect(body.spec.powerState).toBe("Stopped");
    expect(body.spec.reconciliationPolicy).toMatchObject({
      mode: "automatic",
      interval: "30s",
      rolloutWave: 1,
      maxDeferSeconds: 60,
      drainTimeoutSeconds: 120,
    });
    expect(body.spec.className).toBe("prod-large");
    expect(body.spec.backupSpec).toMatchObject({ enabled: true, schedule: "0 2 * * *", retentionDays: 7 });

    vi.unstubAllGlobals();
  });

  // ---- Namespace-aware catalog picker (change F) ----

  const mkItem = (name: string, ns: string, displayName: string) => ({
    name,
    namespace: ns,
    type: "jcasc",
    displayName,
    sourceRef: "test-source",
    valid: true,
  });

  async function openComposePicker(user: ReturnType<typeof userEvent.setup>) {
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("Compose new"));
    await user.click(screen.getByText(/Add catalog item/));
  }

  it("groups catalog picker items by namespace, platform first, with badges", async () => {
    mockListCatalogItems.mockResolvedValue({
      items: [
        mkItem("teamx-item", "team-x", "TeamX Item"),
        mkItem("platform-theme", "varroa-system", "Platform Theme"),
        mkItem("tenant-lib", "varroa", "Tenant Lib"),
      ],
      operatorNamespace: "varroa-system",
    });
    const user = userEvent.setup();
    await openComposePicker(user);

    // Group headers present.
    expect(await screen.findByText(/Platform \/ shared \(varroa-system\)/)).toBeInTheDocument();
    expect(screen.getByText(/varroa \(this controller\)/)).toBeInTheDocument();

    // Platform group renders first.
    const groups = document.querySelectorAll('[data-testid^="picker-group-"]');
    expect(groups.length).toBe(3);
    expect(groups[0].getAttribute("data-testid")).toBe("picker-group-__platform");

    // Items and per-namespace groups render.
    expect(screen.getByText("Platform Theme")).toBeInTheDocument();
    expect(screen.getByTestId("picker-group-team-x")).toBeInTheDocument();
  });

  it("adds an item with its namespace and shows it as a badge on the staged row", async () => {
    mockListCatalogItems.mockResolvedValue({
      items: [mkItem("platform-theme", "varroa-system", "Platform Theme")],
      operatorNamespace: "varroa-system",
    });
    const user = userEvent.setup();
    await openComposePicker(user);

    await user.click(await screen.findByText("Platform Theme"));

    // Picker closed; the staged input row shows the resolved (explicit-namespace)
    // item's display name and a namespace badge.
    expect(await screen.findByText("Platform Theme")).toBeInTheDocument();
    expect(screen.getAllByText("varroa-system").length).toBeGreaterThan(0);
  });

  it("renders a preview warnings banner from the preview response", async () => {
    mockListCatalogItems.mockResolvedValue({
      items: [mkItem("platform-theme", "varroa-system", "Platform Theme")],
      operatorNamespace: "varroa-system",
    });
    mockPreviewComposedBundle.mockResolvedValue({
      jenkinsYaml: "jenkins: {}",
      pluginsYaml: "",
      itemsYaml: "",
      rbacYaml: "",
      missing: [],
      drifted: [],
      warnings: ['itemRef "theme": using varroa/theme; a same-named item exists in the operator namespace (varroa-system/theme) and is shadowed'],
      unresolvedVariables: [],
    });
    const user = userEvent.setup();
    await openComposePicker(user);
    await user.click(await screen.findByText("Platform Theme"));
    await user.click(screen.getByRole("button", { name: /Preview merged JCasC/ }));

    expect(await screen.findByText(/is shadowed/)).toBeInTheDocument();
  });

  it("renders no preview warnings banner when the response has none", async () => {
    mockListCatalogItems.mockResolvedValue({
      items: [mkItem("platform-theme", "varroa-system", "Platform Theme")],
      operatorNamespace: "varroa-system",
    });
    const user = userEvent.setup();
    await openComposePicker(user);
    await user.click(await screen.findByText("Platform Theme"));
    await user.click(screen.getByRole("button", { name: /Preview merged JCasC/ }));

    await waitFor(() => expect(mockPreviewComposedBundle).toHaveBeenCalled());
    expect(screen.queryByText(/is shadowed/)).not.toBeInTheDocument();
  });

  // ---- Multicluster wizard cluster picker (spec: "Wizard cluster picker") ----

  it("shows cluster picker with healthy-only options and core preselect when >= 2 clusters", async () => {
    mockUseClustersData.data = [
      { name: "main", core: true, healthy: true, lastHeartbeat: "2025-01-01T00:00:00Z", operatorVersion: "1.0", k8sVersion: "1.28", controllerCount: 5, connectedCount: 4, state: "active" },
      { name: "dev-cluster", core: false, healthy: true, lastHeartbeat: "2025-01-01T00:00:00Z", operatorVersion: "1.0", k8sVersion: "1.28", controllerCount: 2, connectedCount: 2, state: "active" },
      { name: "edge", core: false, healthy: false, lastHeartbeat: "2025-01-01T00:00:00Z", operatorVersion: "1.0", k8sVersion: "1.28", controllerCount: 1, connectedCount: 0, state: "active" },
    ];
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });

    const selects = screen.getAllByRole("combobox");
    const clusterSelect = selects[0];
    expect(clusterSelect).toBeInTheDocument();

    const options = Array.from(clusterSelect.querySelectorAll("option"));
    const optionValues = options.map((o) => (o as HTMLOptionElement).value);
    const optionTexts = options.map((o) => (o as HTMLOptionElement).textContent);

    expect(optionValues).toEqual(["main", "dev-cluster"]);
    expect(optionValues).not.toContain("edge");
    expect((clusterSelect as HTMLSelectElement).value).toBe("main");
    expect(optionTexts[0]).toMatch(/main.*core/i);
  });

  it("deploys controller to the selected cluster with scoped path and ticket scope", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    mockUseClustersData.data = [
      { name: "main", core: true, healthy: true, lastHeartbeat: "2025-01-01T00:00:00Z", operatorVersion: "1.0", k8sVersion: "1.28", controllerCount: 5, connectedCount: 4, state: "active" },
      { name: "dev-cluster", core: false, healthy: true, lastHeartbeat: "2025-01-01T00:00:00Z", operatorVersion: "1.0", k8sVersion: "1.28", controllerCount: 2, connectedCount: 2, state: "active" },
    ];

    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });

    await user.type(screen.getByPlaceholderText("team-web-builds"), "ci");

    // Pick dev-cluster from the cluster picker (first combobox)
    const clusterSelect = screen.getAllByRole("combobox")[0];
    await user.selectOptions(clusterSelect, "dev-cluster");

    // Navigate to step 2
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    // Step 3
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    // Step 4
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Advanced options").length).toBeGreaterThan(0);
    });
    // Step 5
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });

    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });

    // First arg to createController is the cluster
    const [createCluster] = mockCreateController.mock.calls[0];
    expect(createCluster).toBe("dev-cluster");

    // Ticket scope is controller:dev-cluster/team-a/ci
    await waitFor(() => {
      expect(mockBffFetch).toHaveBeenCalledWith(
        "/stream/ticket",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({ scope: "controller:dev-cluster/varroa/ci" }),
        }),
      );
    });

    vi.unstubAllGlobals();
  });
  it("renders health probes, exercises each probe toggle, and serializes startup values", async () => {
    const user = userEvent.setup();
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });

    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));

    expect(screen.getByText("Health probes")).toBeInTheDocument();
    const startupEnabled = screen.getByLabelText(/enable startup probe/i);
    const readinessEnabled = screen.getByLabelText(/enable readiness probe/i);
    const livenessEnabled = screen.getByLabelText(/enable liveness probe/i);

    await user.type(screen.getByLabelText(/startup initial delay seconds/i), "11");
    await user.type(screen.getByLabelText(/startup period seconds/i), "12");
    await user.type(screen.getByLabelText(/startup timeout seconds/i), "13");
    await user.type(screen.getByLabelText(/startup failure threshold/i), "14");
    await user.type(screen.getByLabelText(/startup success threshold/i), "15");

    const startupToggle = screen.getByLabelText(/enable startup probe/i);
    await user.click(startupToggle);
    expect(screen.getByLabelText(/startup initial delay seconds/i)).toBeDisabled();
    await user.click(startupEnabled);
    expect(screen.getByLabelText(/startup initial delay seconds/i)).toBeEnabled();

    await user.click(readinessEnabled);
    expect(screen.getByLabelText(/readiness initial delay seconds/i)).toBeDisabled();
    await user.click(readinessEnabled);
    expect(screen.getByLabelText(/readiness initial delay seconds/i)).toBeEnabled();

    await user.click(livenessEnabled);
    expect(screen.getByLabelText(/liveness initial delay seconds/i)).toBeDisabled();
    await user.click(livenessEnabled);
    expect(screen.getByLabelText(/liveness initial delay seconds/i)).toBeEnabled();
    await user.click(livenessEnabled);
    expect(screen.getByLabelText(/liveness initial delay seconds/i)).toBeDisabled();

    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });

    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });

    const [, , body] = mockCreateController.mock.calls[0] as [string, string, { spec?: { probes?: unknown } }];
    expect(body.spec?.probes).toEqual({
      startup: {
        initialDelaySeconds: 11,
        periodSeconds: 12,
        timeoutSeconds: 13,
        failureThreshold: 14,
        successThreshold: 15,
      },
      liveness: { disabled: true },
    });
  });

  // ---- Typed pipeline-template variable widgets (add-pipeline-template-catalog-item-type) ----

  const mkItemWithVars = (
    name: string,
    ns: string,
    displayName: string,
    variables: { name: string; type?: string; allowedValues?: string[] }[],
  ) => ({
    name,
    namespace: ns,
    type: "pipeline-template",
    displayName,
    sourceRef: "test-source",
    valid: true,
    variables,
  });

  async function stageItemAndPreviewUnresolved(
    user: ReturnType<typeof userEvent.setup>,
    item: ReturnType<typeof mkItemWithVars>,
    unresolvedVariables: string[],
  ) {
    mockListCatalogItems.mockResolvedValue({ items: [item], operatorNamespace: "varroa-system" });
    mockPreviewComposedBundle.mockResolvedValue({
      jenkinsYaml: "",
      pluginsYaml: "",
      itemsYaml: "items:\n- name: templated-job\n",
      rbacYaml: "",
      missing: [],
      drifted: [],
      warnings: [],
      unresolvedVariables,
    });
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("Compose new"));
    await user.click(screen.getByText(/Add catalog item/));
    await user.click(await screen.findByText(item.displayName));
    await user.click(screen.getByRole("button", { name: /preview merged jcasc/i }));
    await waitFor(() => {
      expect(screen.getByText("Unresolved variables — must be filled before proceeding")).toBeInTheDocument();
    });
  }

  it("string variable with allowedValues renders a dropdown and writes the selected value", async () => {
    const user = userEvent.setup();
    const item = mkItemWithVars("pt-1", "varroa-system", "My Template", [
      { name: "environment", type: "string", allowedValues: ["dev", "prod"] },
    ]);
    await stageItemAndPreviewUnresolved(user, item, ["environment"]);

    const select = screen.getByDisplayValue("select…") as HTMLSelectElement;
    await user.selectOptions(select, "prod");

    expect(select.value).toBe("prod");
  });

  it("number variable renders a number input and writes the typed value", async () => {
    const user = userEvent.setup();
    const item = mkItemWithVars("pt-1", "varroa-system", "My Template", [
      { name: "replicas", type: "number" },
    ]);
    await stageItemAndPreviewUnresolved(user, item, ["replicas"]);

    const input = document.querySelector('input[type="number"]') as HTMLInputElement;
    expect(input).toBeInTheDocument();
    await user.type(input, "3");

    expect(input.value).toBe("3");
  });

  it("boolean variable renders a checkbox and writes true/false strings", async () => {
    const user = userEvent.setup();
    const item = mkItemWithVars("pt-1", "varroa-system", "My Template", [
      { name: "enableFeature", type: "boolean" },
    ]);
    await stageItemAndPreviewUnresolved(user, item, ["enableFeature"]);

    const checkbox = screen.getByRole("checkbox");
    expect(checkbox).not.toBeChecked();
    await user.click(checkbox);
    expect(checkbox).toBeChecked();
  });

  it("credentials variable renders a labeled text input", async () => {
    const user = userEvent.setup();
    const item = mkItemWithVars("pt-1", "varroa-system", "My Template", [
      { name: "deployKey", type: "credentials" },
    ]);
    await stageItemAndPreviewUnresolved(user, item, ["deployKey"]);

    const input = screen.getByPlaceholderText("Jenkins credentials ID");
    expect(input).toBeInTheDocument();
    await user.type(input, "github-deploy-key");
    expect((input as HTMLInputElement).value).toBe("github-deploy-key");
  });

  it("a string variable with no allowedValues falls back to the existing raw editor", async () => {
    const user = userEvent.setup();
    const item = mkItemWithVars("pt-1", "varroa-system", "My Template", [
      { name: "plainVar", type: "string" },
    ]);
    await stageItemAndPreviewUnresolved(user, item, ["plainVar"]);

    expect(screen.getByText(/plainVar/)).toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(document.querySelector('input[type="number"]')).not.toBeInTheDocument();
  });

  it("an unmatched variable falls back to the existing raw key/value row", async () => {
    const user = userEvent.setup();
    const item = mkItemWithVars("pt-1", "varroa-system", "My Template", []);
    await stageItemAndPreviewUnresolved(user, item, ["undeclaredVar"]);

    const pill = screen.getByText(/undeclaredVar/);
    expect(pill).toBeInTheDocument();
    await userEvent.setup().click(pill);
    // Clicking seeds a blank row in the generic "Bundle variables" editor.
    await waitFor(() => {
      expect(screen.getAllByPlaceholderText("key").length).toBeGreaterThan(1);
    });
  });

  // ---- Mite image / pull policy controls (add-mite-image-controls #376) ----

  async function navigateToAdvancedOptions(user: ReturnType<typeof userEvent.setup>) {
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.queryByText("Loading configuration...")).not.toBeInTheDocument();
    });
    await user.type(screen.getByPlaceholderText("team-web-builds"), "my-ctrl");
    await user.click(screen.getByRole("button", { name: /continue.*bundle/i }));
    await user.click(screen.getByText("None"));
    await user.click(screen.getByRole("button", { name: /continue.*resources/i }));
    await user.type(screen.getByPlaceholderText("2"), "2");
    await user.type(screen.getByPlaceholderText("4Gi"), "4Gi");
    await user.type(screen.getByPlaceholderText("20Gi"), "20Gi");
    await user.click(screen.getByRole("button", { name: /continue.*advanced options/i }));
  }

  it("populates miteSpec.image when miteImage is set and omits imagePullPolicy when unset", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    const user = userEvent.setup();
    await navigateToAdvancedOptions(user);

    await user.type(screen.getByPlaceholderText("image (optional)"), "ghcr.io/varroaci/varroa-jenkins:v2");
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });
    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });

    const [, , body] = mockCreateController.mock.calls[0] as [string, string, { spec?: { miteSpec?: Record<string, unknown> } }];
    expect(body.spec?.miteSpec).toBeDefined();
    expect(body.spec!.miteSpec!.image).toBe("ghcr.io/varroaci/varroa-jenkins:v2");
    expect(body.spec!.miteSpec!.imagePullPolicy).toBeUndefined();
    expect(body.spec!.miteSpec!.resources).toBeUndefined();

    vi.unstubAllGlobals();
  });

  it("populates miteSpec.imagePullPolicy when set and omits image when unset", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    const user = userEvent.setup();
    await navigateToAdvancedOptions(user);

    await user.selectOptions(
      screen.getByDisplayValue("Unset (default)"),
      "Always",
    );
    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });
    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });

    const [, , body] = mockCreateController.mock.calls[0] as [string, string, { spec?: { miteSpec?: Record<string, unknown> } }];
    expect(body.spec?.miteSpec).toBeDefined();
    expect(body.spec!.miteSpec!.imagePullPolicy).toBe("Always");
    expect(body.spec!.miteSpec!.image).toBeUndefined();
    expect(body.spec!.miteSpec!.resources).toBeUndefined();

    vi.unstubAllGlobals();
  });

  it("merges all 4 mite fields into a single miteSpec object", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    const user = userEvent.setup();
    await navigateToAdvancedOptions(user);

    await user.type(screen.getByPlaceholderText("image (optional)"), "ghcr.io/varroaci/varroa-jenkins:v3");
    await user.selectOptions(
      screen.getByDisplayValue("Unset (default)"),
      "Never",
    );
    await user.type(screen.getByPlaceholderText("cpu"), "250m");
    await user.type(screen.getByPlaceholderText("memory"), "512Mi");

    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });
    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });

    const [, , body] = mockCreateController.mock.calls[0] as [string, string, { spec?: { miteSpec?: Record<string, unknown> } }];
    expect(body.spec?.miteSpec).toBeDefined();
    expect(body.spec!.miteSpec!.image).toBe("ghcr.io/varroaci/varroa-jenkins:v3");
    expect(body.spec!.miteSpec!.imagePullPolicy).toBe("Never");
    expect(body.spec!.miteSpec!.resources).toEqual({
      requests: { cpu: "250m", memory: "512Mi" },
    });

    vi.unstubAllGlobals();
  });

  it("omits image and imagePullPolicy from payload when both are left unset", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    const user = userEvent.setup();
    await navigateToAdvancedOptions(user);

    // Leave miteImage and miteImagePullPolicy at their default empty values.
    // Only set mite CPU to ensure miteSpec appears but without image keys.
    await user.type(screen.getByPlaceholderText("cpu"), "100m");

    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });
    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });

    const [, , body] = mockCreateController.mock.calls[0] as [string, string, { spec?: { miteSpec?: Record<string, unknown> } }];
    expect(body.spec?.miteSpec).toBeDefined();
    expect(body.spec!.miteSpec!.resources).toBeDefined(); // cpu set
    expect(body.spec!.miteSpec!.image).toBeUndefined();
    expect(body.spec!.miteSpec!.imagePullPolicy).toBeUndefined();

    vi.unstubAllGlobals();
  });

  it("omits miteSpec entirely when no mite fields are set", async () => {
    class MockEventSource {
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      close = vi.fn();
      constructor(public url: string) {}
    }
    vi.stubGlobal("EventSource", MockEventSource);

    const user = userEvent.setup();
    await navigateToAdvancedOptions(user);

    await user.click(screen.getByRole("button", { name: /continue.*review/i }));
    await waitFor(() => {
      expect(screen.getAllByText("Review & deploy").length).toBeGreaterThan(0);
    });
    await user.click(screen.getByRole("button", { name: /deploy controller/i }));
    await waitFor(() => {
      expect(mockCreateController).toHaveBeenCalled();
    });

    const [, , body] = mockCreateController.mock.calls[0] as [string, string, { spec?: { miteSpec?: unknown } }];
    expect(body.spec?.miteSpec).toBeUndefined();

    vi.unstubAllGlobals();
  });
});
