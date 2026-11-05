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
        provisioningTimeoutSec: 600,
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
    expect(screen.getByRole("tab", { name: "Provisioning" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Versions" })).toBeInTheDocument();
    // Admin-only tabs must not be present.
    expect(screen.queryByRole("tab", { name: "Users" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Groups" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Identity" })).not.toBeInTheDocument();
  });

  it("hides the Versions tab when the user lacks provisioningdefaults update", async () => {
    // No permissions → neither the Provisioning nor the Versions tab (both are
    // gated on provisioningdefaults:update).
    mockCanDo.mockReturnValue(false);
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByRole("heading", { name: "Settings" });
    expect(screen.queryByRole("tab", { name: "Provisioning" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Versions" })).not.toBeInTheDocument();
  });

  it("shows no default plugins or storage size field", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();
    await screen.findByText("Default Resources");
    expect(screen.getByLabelText("Storage Class")).toBeInTheDocument();
    expect(screen.queryByText("Default Plugins")).not.toBeInTheDocument();
    expect(screen.queryByText("+ Add Plugin")).not.toBeInTheDocument();
    expect(screen.queryByPlaceholderText("artifactId")).not.toBeInTheDocument();
    expect(screen.queryByText("Storage Size (GB)")).not.toBeInTheDocument();
  });

  it("saves the spec without defaultPlugins or storageSizeGB", async () => {
    const config = createProvisioningConfig({ spec: { storageClass: "fast", defaultStorage: "20Gi" } });
    mockGetConfig.mockResolvedValue(config);
    mockUpdateConfig.mockResolvedValue(config);
    renderSettings();
    await userEvent.click(await screen.findByText("Save"));
    await waitFor(() => expect(mockUpdateConfig).toHaveBeenCalled());
    const savedSpec = mockUpdateConfig.mock.calls[0][1].spec;
    expect(savedSpec.storageClass).toBe("fast");
    expect(savedSpec.defaultStorage).toBe("20Gi");
    // JSON drops undefined values, so this matches what the server receives.
    expect(JSON.stringify(savedSpec)).not.toMatch(/defaultPlugins|storageSizeGB/);
  });

  it("renders ingress annotations section", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();
    await screen.findByText("Ingress Annotations");
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

  it("shows the provisioning timeout and its hint", async () => {
    mockGetConfig.mockResolvedValue(
      createProvisioningConfig({ spec: { provisioningTimeoutSec: 900 } }),
    );
    renderSettings();

    await screen.findByText("Provisioning Timeout (seconds)");
    expect(screen.getByDisplayValue("900")).toBeInTheDocument();
    expect(screen.getByText(/Image pull time does not count/)).toBeInTheDocument();
  });

  it("saves the provisioning timeout as a number", async () => {
    const config = createProvisioningConfig({ spec: { provisioningTimeoutSec: 600 } });
    mockGetConfig.mockResolvedValue(config);
    mockUpdateConfig.mockResolvedValue(config);
    renderSettings();

    const input = await screen.findByLabelText("Provisioning Timeout (seconds)");
    await userEvent.clear(input);
    await userEvent.type(input, "900");
    await userEvent.click(screen.getByText("Save"));

    await waitFor(() => {
      expect(mockUpdateConfig).toHaveBeenCalled();
    });
    expect(mockUpdateConfig.mock.calls[0][1].spec.provisioningTimeoutSec).toBe(900);
  });

  it("clears the provisioning timeout to the default", async () => {
    const config = createProvisioningConfig({ spec: { provisioningTimeoutSec: 600 } });
    mockGetConfig.mockResolvedValue(config);
    mockUpdateConfig.mockResolvedValue(config);
    renderSettings();

    const input = await screen.findByLabelText("Provisioning Timeout (seconds)");
    await userEvent.clear(input);
    await userEvent.click(screen.getByText("Save"));

    await waitFor(() => {
      expect(mockUpdateConfig).toHaveBeenCalled();
    });
    expect(mockUpdateConfig.mock.calls[0][1].spec.provisioningTimeoutSec).toBeUndefined();
  });

  it("renders root domain and version fields", async () => {
    mockGetConfig.mockResolvedValue(createProvisioningConfig());
    renderSettings();

    await screen.findByText("Root Domain");
    expect(screen.getByText("Default Jenkins Version")).toBeInTheDocument();
  });
});
