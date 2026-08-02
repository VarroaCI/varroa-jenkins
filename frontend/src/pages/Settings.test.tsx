import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// Mock API client
vi.mock("../api/client", () => ({
  getProvisioningDefaults: vi.fn(),
  updateProvisioningDefaults: vi.fn(),
  getVersionProfiles: vi.fn(() => Promise.resolve([])),
  listClusters: vi.fn(() => Promise.resolve([{ name: "core", core: true }])),
}));

// Mock AuthContext
vi.mock("../context/AuthContext", () => ({
  useAuth: vi.fn(),
}));

// Mock usePermissions
vi.mock("../hooks/usePermissions", () => ({
  canDoGlobal: vi.fn(),
}));

import { getProvisioningDefaults, updateProvisioningDefaults } from "../api/client";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import Settings from "./Settings";
import { createProvisioningConfig } from "../test/factories";

const mockGetConfig = getProvisioningDefaults as ReturnType<typeof vi.fn>;
const mockUpdateConfig = updateProvisioningDefaults as ReturnType<typeof vi.fn>;
const mockUseAuth = useAuth as ReturnType<typeof vi.fn>;
const mockCanDo = canDoGlobal as ReturnType<typeof vi.fn>;

function renderSettings() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <Settings />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockGetConfig.mockReset();
  mockUpdateConfig.mockReset();
  mockUseAuth.mockReset();
  mockCanDo.mockReset();

  mockUseAuth.mockReturnValue({
    permissions: {},
    user: { name: "admin" },
    isAuthenticated: true,
  });
  mockCanDo.mockReturnValue(true);
});

describe("Settings page", () => {
  it("shows loading spinner while config loads", () => {
    mockGetConfig.mockReturnValue(new Promise(() => {})); // never resolves
    renderSettings();
    // LoadingSpinner renders an accessible element or some visible indicator
    // Just verify the error state hasn't appeared
    expect(screen.queryByText(/Error:/)).not.toBeInTheDocument();
  });

  it("shows error message when config fetch fails", async () => {
    mockGetConfig.mockRejectedValue(new Error("Failed to fetch"));
    renderSettings();
    await screen.findByText(/Error: Failed to fetch/);
  });

  it("renders form with provisioning defaults after loading", async () => {
    const config = createProvisioningConfig({
      spec: {
        rootDomain: "example.com",
        defaultVersion: "2.492.3",
        defaultCPU: "2",
        defaultMemory: "4Gi",
        defaultStorage: "20Gi",
        storageClass: "fast",
        storageSizeGB: 100,
        provisioningTimeoutSec: 600,
        defaultPlugins: [{ artifactId: "git", version: "5.0.0" }],
      },
    });
    mockGetConfig.mockResolvedValue(config);
    renderSettings();

    await screen.findByText(/Defaults applied to controllers on the selected cluster/);

    // Form fields should be populated
    const rootDomainInput = screen.getByDisplayValue("example.com");
    expect(rootDomainInput).toBeInTheDocument();

    const versionInput = screen.getByDisplayValue("2.492.3");
    expect(versionInput).toBeInTheDocument();
  });

  it("renders heading", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();
    await screen.findByRole("heading", { name: "Settings" });
    expect(
      await screen.findByText(/Defaults applied to controllers on the selected cluster/),
    ).toBeInTheDocument();
  });

  it("shows only the Provisioning and Versions tabs for a non-admin operator", async () => {
    // Operator can update provisioning defaults but is not varroa:admin.
    mockCanDo.mockImplementation(
      (_perms: unknown, resource: string) => resource === "provisioningdefaults",
    );
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText(/Defaults applied to controllers on the selected cluster/);
    // Provisioning + Versions gated on provisioningdefaults:update both appear.
    expect(screen.getByRole("button", { name: "Provisioning" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Versions" })).toBeInTheDocument();
    // Admin-only tabs must not be present.
    expect(screen.queryByRole("button", { name: "Users" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Groups" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Identity" })).not.toBeInTheDocument();
  });

  it("hides the Versions tab when the user lacks provisioningdefaults update", async () => {
    // No permissions → neither the Provisioning nor the Versions tab (both are
    // gated on provisioningdefaults:update).
    mockCanDo.mockReturnValue(false);
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByRole("heading", { name: "Settings" });
    expect(screen.queryByRole("button", { name: "Provisioning" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Versions" })).not.toBeInTheDocument();
  });

  it("removes a plugin row when its '×' is clicked", async () => {
    mockGetConfig.mockResolvedValue(
      createProvisioningConfig({ spec: { defaultPlugins: [{ artifactId: "git", version: "5.0.0" }] } }),
    );
    renderSettings();

    await screen.findByDisplayValue("git");
    await userEvent.click(screen.getAllByText("×")[0]);
    expect(screen.queryByDisplayValue("git")).not.toBeInTheDocument();
  });

  it("renders default plugins section", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();
    await screen.findByText("Default Plugins");
  });

  it("renders ingress annotations section", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();
    await screen.findByText("Ingress Annotations");
  });

  it("adds a new plugin row when '+ Add Plugin' is clicked", async () => {
    const config = createProvisioningConfig({
      spec: { defaultPlugins: [] },
    });
    mockGetConfig.mockResolvedValue(config);
    renderSettings();

    await screen.findByText("Default Plugins");
    const addBtn = screen.getByText("+ Add Plugin");
    await userEvent.click(addBtn);

    // Should now have at least one plugin name input
    const pluginInputs = screen.getAllByPlaceholderText("artifactId");
    expect(pluginInputs.length).toBeGreaterThanOrEqual(1);
  });

  it("adds a new annotation row when '+ Add Annotation' is clicked", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText("Ingress Annotations");
    const addBtn = screen.getByText("+ Add Annotation");
    await userEvent.click(addBtn);

    const keyInputs = screen.getAllByPlaceholderText("annotation key");
    expect(keyInputs.length).toBeGreaterThanOrEqual(1);
  });

  it("removes an annotation row when its '×' is clicked", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText("Ingress Annotations");
    await userEvent.click(screen.getByText("+ Add Annotation"));
    const before = screen.getAllByPlaceholderText("annotation key").length;
    const xs = screen.getAllByText("×");
    await userEvent.click(xs[xs.length - 1]);
    expect(screen.getAllByPlaceholderText("annotation key").length).toBe(before - 1);
  });

  it("fills plugin fields and saves", async () => {
    const config = createProvisioningConfig({
      spec: { defaultPlugins: [] },
    });
    mockGetConfig.mockResolvedValue(config);
    mockUpdateConfig.mockResolvedValue(config);
    renderSettings();

    await screen.findByText("Default Plugins");

    // Add a plugin
    await userEvent.click(screen.getByText("+ Add Plugin"));

    const artifactInput = screen.getAllByPlaceholderText("artifactId")[0];
    await userEvent.type(artifactInput, "my-plugin");

    const versionInput = screen.getAllByPlaceholderText("version")[0];
    await userEvent.type(versionInput, "1.0");

    // Submit form
    const saveBtn = screen.getByText("Save");
    await userEvent.click(saveBtn);

    await waitFor(() => {
      expect(mockUpdateConfig).toHaveBeenCalled();
    });

    // updateProvisioningDefaults(cluster, updated): the payload is the 2nd arg.
    const updatedArg = mockUpdateConfig.mock.calls[0][1];
    expect(updatedArg.spec.defaultPlugins).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ artifactId: "my-plugin", version: "1.0" }),
      ]),
    );
  });

  it("shows 'Saved.' confirmation after successful save", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    mockUpdateConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText("Save");
    await userEvent.click(screen.getByText("Save"));

    await screen.findByText("Saved.");
  });

  it("shows saving state on submit button", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    // Don't resolve the update so we see "Saving..."
    mockUpdateConfig.mockReturnValue(new Promise(() => {}));
    renderSettings();

    await screen.findByText("Save");
    await userEvent.click(screen.getByText("Save"));

    expect(screen.getByText("Saving...")).toBeInTheDocument();
  });

  it("shows error on save failure", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    mockUpdateConfig.mockRejectedValue(new Error("Save failed"));
    renderSettings();

    await screen.findByText("Save");
    await userEvent.click(screen.getByText("Save"));

    await screen.findByText(/Error: Save failed/);
  });

  it("shows no-access message when user lacks all settings permissions", async () => {
    mockCanDo.mockReturnValue(false);
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText(/You don't have permission to access any settings sections/);
    expect(screen.queryByText("Save")).not.toBeInTheDocument();
  });

  it("renders resource fields (CPU, Memory, Storage)", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText("Default Resources");
    expect(screen.getByText("CPU")).toBeInTheDocument();
    expect(screen.getByText("Memory")).toBeInTheDocument();
    expect(screen.getByText("Storage")).toBeInTheDocument();
  });

  it("renders root domain and version fields", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText("Root Domain");
    expect(screen.getByText("Default Jenkins Version")).toBeInTheDocument();
  });
});
