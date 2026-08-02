import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
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

vi.mock("../hooks/useClusters", () => ({ useClusters: () => ({ data: [{name:"core",core:true,healthy:true,state:"active",lastHeartbeat:"2025-01-01T00:00:00Z",operatorVersion:"1.0",k8sVersion:"1.28",controllerCount:5,connectedCount:4}], isLoading: false, isError: false }), coreOf: (c: unknown[]) => c?.find((c2: any) => c2.core) }));

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

const BASE_CONFIG = {
  rootDomain: "jenkins.example.com",
  defaultNamespace: "varroa",
  namespaces: ["varroa", "varroa-tenants"],
  defaultVersion: "2.479.3",
  versions: [
    { version: "2.479.3", channel: "lts", recommended: true, eol: "" },
  ],
  sizePresets: [],
  injectedVariables: [],
};

beforeEach(() => {
  mockNavigate.mockReset();
  mockGetProvisioningConfig.mockResolvedValue(BASE_CONFIG);
  mockGetController.mockRejectedValue(new Error("not found"));
  mockListComposedBundles.mockResolvedValue({ items: [] });
  mockListCatalogItems.mockResolvedValue({ items: [] });
  mockListGroups.mockResolvedValue([]);
  mockPreviewComposedBundle.mockResolvedValue({
    jenkinsYaml: "", pluginsYaml: "", itemsYaml: "", rbacYaml: "",
    missing: [], drifted: [], warnings: [], unresolvedVariables: [],
  });
  mockPreflightController.mockResolvedValue({ checks: [] });
  mockRenderController.mockResolvedValue("");
  mockCreateController.mockResolvedValue({});
});

describe("ControllerWizard deployable namespaces", () => {
  it("renders a <select> when allowFreeform is false with namespaces", async () => {
    mockGetDeployableNamespaces.mockResolvedValue({
      namespaces: ["team-a"],
      defaultNamespace: "team-a",
      allowFreeform: false,
      degraded: false,
    });
    render(<ControllerWizard />);
    // Wait for deployable namespaces to load (seeded from cluster picker)
    await waitFor(() => {
      const el = screen.queryByRole("combobox") as HTMLSelectElement | null;
      expect(el?.value).toBe("team-a");
    }, { timeout: 3000 });
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    expect(select.value).toBe("team-a");
    // Should have team-a as an option
    expect(screen.getByText("team-a")).toBeInTheDocument();
  });

  it("renders a free-text input when allowFreeform is true", async () => {
    mockGetDeployableNamespaces.mockResolvedValue({
      namespaces: [],
      defaultNamespace: "",
      allowFreeform: true,
      degraded: false,
    });
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(screen.getByPlaceholderText("varroa-tenants")).toBeInTheDocument();
    }, { timeout: 3000 });
    const input = screen.getByPlaceholderText("varroa-tenants");
    expect(input).toHaveAttribute("list", "deployable-ns-suggestions");
  });

  it("shows not-authorized message when namespaces is empty and allowFreeform is false", async () => {
    mockGetDeployableNamespaces.mockResolvedValue({
      namespaces: [],
      defaultNamespace: "",
      allowFreeform: false,
      degraded: false,
    });
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(
        screen.getByText("You are not authorized to create controllers in any namespace."),
      ).toBeInTheDocument();
    }, { timeout: 3000 });
    // ...and the namespace select is disabled, since there is nothing to pick.
    expect((screen.getByRole("combobox") as HTMLSelectElement).disabled).toBe(true);
  });

  it("shows degraded warning when deployableNs.degraded is true", async () => {
    mockGetDeployableNamespaces.mockResolvedValue({
      namespaces: [],
      defaultNamespace: "",
      allowFreeform: true,
      degraded: true,
    });
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(
        screen.getByText(/cluster.*unreachable.*namespace suggestions/i)
      ).toBeInTheDocument();
    }, { timeout: 3000 });
  });

  it("renders degraded on fetch rejection", async () => {
    mockGetDeployableNamespaces.mockRejectedValue(new Error("network error"));
    render(<ControllerWizard />);
    await waitFor(() => {
      expect(
        screen.getByText(/cluster.*unreachable.*namespace suggestions/i)
      ).toBeInTheDocument();
    }, { timeout: 3000 });
  });
});
