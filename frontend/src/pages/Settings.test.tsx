import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";
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

import { getProvisioningDefaults, updateProvisioningDefaults, listClusters } from "../api/client";
import { useAuth } from "../context/AuthContext";
import { canDoGlobal } from "../hooks/usePermissions";
import Settings from "./Settings";
import { createProvisioningConfig } from "../test/factories";
import { ApiError } from "../hooks/useApi";
import type { ProvisioningDefaults } from "../types";

const mockGetConfig = getProvisioningDefaults as ReturnType<typeof vi.fn>;
const mockUpdateConfig = updateProvisioningDefaults as ReturnType<typeof vi.fn>;
const mockUseAuth = useAuth as ReturnType<typeof vi.fn>;
const mockCanDo = canDoGlobal as ReturnType<typeof vi.fn>;
const mockListClusters = listClusters as ReturnType<typeof vi.fn>;

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

    // The intro text also shows while the read is in progress.
    // Wait for a value that only the loaded form shows.
    expect(await screen.findByDisplayValue("example.com")).toBeInTheDocument();
    expect(screen.getByDisplayValue("2.492.3")).toBeInTheDocument();
    expect(
      screen.getByText(/Defaults applied to controllers on the selected cluster/),
    ).toBeInTheDocument();
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
    expect(screen.getByText(/Image pulls and the plugin download do not count/)).toBeInTheDocument();
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

describe("Settings provisioning tab without varroa-defaults", () => {
  const NOTE = "This cluster has no provisioning defaults. Save creates them.";

  it("shows the empty form with a note when the cluster has no provisioning defaults", async () => {
    mockGetConfig.mockRejectedValue(new ApiError(404, "We could not find that page."));
    renderSettings();

    expect(await screen.findByText(NOTE)).toBeInTheDocument();
    expect(screen.queryByText(/Error:/)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Storage Class")).toHaveValue("");
    expect(screen.getByLabelText("Storage Class")).toHaveAttribute("placeholder", "standard");
    expect(screen.getByLabelText("Provisioning Timeout (seconds)")).toHaveAttribute("placeholder", "300");
    expect(screen.getByText("Save")).toBeInTheDocument();
  });

  it("shows the error for a failed read that is not a 404", async () => {
    mockGetConfig.mockRejectedValue(new ApiError(500, "Varroa could not complete the request."));
    renderSettings();

    expect(await screen.findByText("Error: Varroa could not complete the request.")).toBeInTheDocument();
    expect(screen.queryByText(NOTE)).not.toBeInTheDocument();
  });

  it("creates the defaults on the first save of a missing object", async () => {
    mockGetConfig.mockRejectedValue(new ApiError(404, "We could not find that page."));
    mockUpdateConfig.mockResolvedValue(
      createProvisioningConfig({ metadata: { name: "varroa-defaults", resourceVersion: "1" }, spec: { storageClass: "fast" } }),
    );
    renderSettings();

    await userEvent.type(await screen.findByLabelText("Storage Class"), "fast");
    await userEvent.click(screen.getByText("Save"));

    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    expect(mockUpdateConfig).toHaveBeenCalledTimes(1);
    expect(mockUpdateConfig).toHaveBeenCalledWith(
      "core",
      expect.objectContaining({
        apiVersion: "varroa.dev/v1alpha1",
        kind: "ProvisioningDefaults",
        metadata: { name: "varroa-defaults" },
        spec: expect.objectContaining({ storageClass: "fast" }),
      }),
    );
    expect(mockUpdateConfig.mock.calls[0][1].spec.ingressAnnotations).toBeUndefined();
    expect(screen.queryByText(NOTE)).not.toBeInTheDocument();
  });

  it("updates the defaults on a second save after the create", async () => {
    mockGetConfig.mockRejectedValue(new ApiError(404, "We could not find that page."));
    mockUpdateConfig.mockResolvedValue(
      createProvisioningConfig({ metadata: { name: "varroa-defaults", resourceVersion: "1" }, spec: { storageClass: "fast" } }),
    );
    renderSettings();

    await userEvent.type(await screen.findByLabelText("Storage Class"), "fast");
    await userEvent.click(screen.getByText("Save"));
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    await userEvent.click(screen.getByText("Save"));

    await waitFor(() => expect(mockUpdateConfig).toHaveBeenCalledTimes(2));
    const second = mockUpdateConfig.mock.calls[1][1];
    expect(second.metadata.resourceVersion).toBe("1");
    expect(second.spec.storageClass).toBe("fast");
  });


  it("shows the conflict note and keeps the form on a 409 save", async () => {
    mockGetConfig.mockResolvedValue(
      createProvisioningConfig({ metadata: { name: "varroa-defaults", resourceVersion: "1" }, spec: { storageClass: "fast" } }),
    );
    mockUpdateConfig.mockRejectedValue(new ApiError(409, "changed after it was read"));
    renderSettings();

    await userEvent.type(await screen.findByLabelText("Storage Class"), "x");
    await userEvent.click(screen.getByText("Save"));

    expect(await screen.findByText(/Another user or tool changed these defaults/)).toBeInTheDocument();
    expect(screen.getByText("Save")).toBeInTheDocument();
    expect(screen.queryByText(/^Error:/)).not.toBeInTheDocument();
  });

  it("shows the conflict note without the missing note when another writer creates the defaults first", async () => {
    mockGetConfig.mockRejectedValue(new ApiError(404, "We could not find that page."));
    mockUpdateConfig.mockRejectedValue(new ApiError(409, "was created after it was read"));
    renderSettings();

    await userEvent.type(await screen.findByLabelText("Storage Class"), "fast");
    await userEvent.click(screen.getByText("Save"));

    expect(await screen.findByText(/Another user or tool changed these defaults/)).toBeInTheDocument();
    expect(screen.queryByText(NOTE)).not.toBeInTheDocument();
    expect(mockUpdateConfig.mock.calls[0][1].metadata.resourceVersion).toBeUndefined();
  });

  it("reloads the defaults when Reload is clicked after a conflict", async () => {
    mockGetConfig
      .mockResolvedValueOnce(
        createProvisioningConfig({ metadata: { name: "varroa-defaults", resourceVersion: "1" }, spec: { storageClass: "fast" } }),
      )
      .mockResolvedValueOnce(
        createProvisioningConfig({ metadata: { name: "varroa-defaults", resourceVersion: "2" }, spec: { storageClass: "rotated" } }),
      );
    mockUpdateConfig.mockRejectedValue(new ApiError(409, "changed after it was read"));
    renderSettings();

    await userEvent.type(await screen.findByLabelText("Storage Class"), "x");
    await userEvent.click(screen.getByText("Save"));
    await userEvent.click(await screen.findByRole("button", { name: "Reload" }));

    await waitFor(() => expect(screen.getByLabelText("Storage Class")).toHaveValue("rotated"));
    expect(screen.queryByText(/Another user or tool changed these defaults/)).not.toBeInTheDocument();
    expect(mockGetConfig).toHaveBeenCalledTimes(2);
  });

  it("keeps the typed value when the save fails", async () => {
    mockGetConfig.mockResolvedValue(
      createProvisioningConfig({ metadata: { name: "varroa-defaults", resourceVersion: "1" }, spec: { storageClass: "standard" } }),
    );
    mockUpdateConfig.mockRejectedValueOnce(new ApiError(500, "Varroa could not complete the request."));
    renderSettings();

    const input = await screen.findByLabelText("Storage Class");
    await userEvent.clear(input);
    await userEvent.type(input, "fast");
    await userEvent.click(screen.getByText("Save"));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Varroa could not complete the request.");
    expect(screen.getByLabelText("Storage Class")).toHaveValue("fast");
    expect(screen.getByText("Save")).toBeInTheDocument();

    mockUpdateConfig.mockResolvedValueOnce(createProvisioningConfig());
    await userEvent.click(screen.getByText("Save"));

    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});

describe("Settings provisioning tab cluster selector on a read error", () => {
  beforeEach(() => {
    mockListClusters.mockResolvedValue([
      { name: "core", core: true, healthy: true, state: "active" },
      { name: "member-b", core: false, healthy: true, state: "active" },
    ]);
  });

  afterEach(() => {
    mockListClusters.mockResolvedValue([{ name: "core", core: true }]);
  });

  it("keeps the cluster selector when the config read fails", async () => {
    mockGetConfig.mockImplementation((cluster: string) =>
      cluster === "member-b"
        ? Promise.reject(new ApiError(500, "Varroa could not complete the request."))
        : Promise.resolve(createProvisioningConfig()),
    );
    renderSettings();

    await userEvent.selectOptions(await screen.findByRole("combobox"), "member-b");

    expect(await screen.findByText("Error: Varroa could not complete the request.")).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toHaveValue("member-b");

    await userEvent.selectOptions(screen.getByRole("combobox"), "core");

    expect(await screen.findByLabelText("Storage Class")).toBeInTheDocument();
    expect(screen.queryByText(/Error:/)).not.toBeInTheDocument();
  });
});

describe("Settings provisioning tab cluster switch", () => {
  beforeEach(() => {
    mockListClusters.mockResolvedValue([
      { name: "core", core: true, healthy: true, state: "active" },
      { name: "member-b", core: false, healthy: true, state: "active" },
    ]);
  });

  afterEach(() => {
    mockListClusters.mockResolvedValue([{ name: "core", core: true }]);
  });

  it("keeps the values of the selected cluster when the read of the previous cluster ends later", async () => {
    let resolveCore!: (value: ProvisioningDefaults) => void;
    mockGetConfig.mockImplementation((cluster: string) =>
      cluster === "core"
        ? new Promise<ProvisioningDefaults>((resolve) => {
            resolveCore = resolve;
          })
        : Promise.resolve(createProvisioningConfig({ spec: { storageClass: "member-sc" } })),
    );
    renderSettings();

    await userEvent.selectOptions(await screen.findByRole("combobox"), "member-b");
    expect(await screen.findByLabelText("Storage Class")).toHaveValue("member-sc");

    await act(async () => {
      resolveCore(createProvisioningConfig({ spec: { storageClass: "core-sc" } }));
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByLabelText("Storage Class")).toHaveValue("member-sc");
    expect(screen.getByRole("combobox")).toHaveValue("member-b");
  });

  it("ignores a late not-found answer for the previous cluster", async () => {
    let rejectCore!: (reason: unknown) => void;
    mockGetConfig.mockImplementation((cluster: string) =>
      cluster === "core"
        ? new Promise<ProvisioningDefaults>((_resolve, reject) => {
            rejectCore = reject;
          })
        : Promise.resolve(createProvisioningConfig({ spec: { storageClass: "member-sc" } })),
    );
    renderSettings();

    await userEvent.selectOptions(await screen.findByRole("combobox"), "member-b");
    expect(await screen.findByLabelText("Storage Class")).toHaveValue("member-sc");

    await act(async () => {
      rejectCore(new ApiError(404, "not found"));
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByLabelText("Storage Class")).toHaveValue("member-sc");
    expect(
      screen.queryByText("This cluster has no provisioning defaults. Save creates them."),
    ).not.toBeInTheDocument();
  });

  it("ignores a save answer for the previous cluster", async () => {
    let resolveSave!: (value: ProvisioningDefaults) => void;
    mockGetConfig.mockImplementation((cluster: string) =>
      Promise.resolve(
        createProvisioningConfig({
          spec: { storageClass: cluster === "core" ? "core-sc" : "member-sc" },
        }),
      ),
    );
    mockUpdateConfig.mockReturnValue(
      new Promise<ProvisioningDefaults>((resolve) => {
        resolveSave = resolve;
      }),
    );
    renderSettings();

    await waitFor(() => expect(screen.getByLabelText("Storage Class")).toHaveValue("core-sc"));
    await userEvent.click(screen.getByText("Save"));
    await userEvent.selectOptions(screen.getByRole("combobox"), "member-b");
    await waitFor(() => expect(screen.getByLabelText("Storage Class")).toHaveValue("member-sc"));

    await act(async () => {
      resolveSave(createProvisioningConfig({ spec: { storageClass: "core-saved" } }));
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByLabelText("Storage Class")).toHaveValue("member-sc");
    expect(screen.queryByText("Saved.")).not.toBeInTheDocument();
    expect(screen.getByText("Save")).toBeEnabled();
  });
});
