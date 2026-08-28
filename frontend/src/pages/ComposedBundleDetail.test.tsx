import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import ComposedBundleDetail from "./ComposedBundleDetail";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false } },
});

function renderWithProviders(ui: React.ReactElement) {
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/catalog/bundles/default/test-bundle"]}>
        {ui}
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ namespace: "default", name: "test-bundle" }),
    useNavigate: () => mockNavigate,
  };
});

const mockGetComposedBundle = vi.fn();
const mockDeleteComposedBundle = vi.fn();
const mockUpdateController = vi.fn();
vi.mock("../hooks/useClusters", () => ({ useClusters: () => ({ data: [{name:"core",core:true,healthy:true,state:"active",lastHeartbeat:"2025-01-01T00:00:00Z",operatorVersion:"1.0",k8sVersion:"1.28",controllerCount:5,connectedCount:4}], isLoading: false, isError: false }), coreOf: (c: unknown[]) => c?.find((c2: any) => c2.core) }));

vi.mock("../api/client", () => ({
  listClusters: vi.fn(),
  getComposedBundle: (...args: unknown[]) => mockGetComposedBundle(...args),
  deleteComposedBundle: (...args: unknown[]) => mockDeleteComposedBundle(...args),
  updateController: (...args: unknown[]) => mockUpdateController(...args),
}));

// Mock useComposer
const mockComposer = {
  items: [],
  variables: {},
  addItem: vi.fn(),
  removeItem: vi.fn(),
  reorderItem: vi.fn(),
  setVar: vi.fn(),
  clear: vi.fn(),
  hasItem: vi.fn(),
  toSpec: vi.fn(),
};
vi.mock("../context/ComposerContext", () => ({
  useComposer: () => mockComposer,
}));

// Mock useControllers
const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
}));

// Mock usePermissions — use a mutable variable so tests can override canDoAnywhere
let mockCanDoFn: (...args: unknown[]) => boolean = () => true;
vi.mock("../hooks/usePermissions", () => ({
  usePermissions: () => ({
    data: {
      global: {
        composedbundles: { update: true, delete: true, get: true, list: true },
        controllers: { update: true },
      },
      scopes: [],
    },
  }),
  canDoAnywhere: (...args: unknown[]) => mockCanDoFn(...args),
}));

// Mock useToast
vi.mock("../components/Toast", () => ({
  useToast: () => ({ toast: vi.fn() }),
}));

// Mock BundleHealthBadge
vi.mock("../components/BundleSelector", () => ({
  BundleHealthBadge: ({ phase }: { phase?: string }) => (
    <span data-testid={`badge-${phase ?? "none"}`}>[{phase ?? "no-phase"}]</span>
  ),
}));

const bundleFixture = (overrides?: Record<string, unknown>) => ({
  apiVersion: "varroa.dev/v1alpha1",
  kind: "ComposedBundle",
  metadata: {
    name: "test-bundle",
    namespace: "default",
    creationTimestamp: "2024-06-01T00:00:00Z",
    ...(overrides?.metadata as Record<string, unknown> ?? {}),
  },
  spec: {
    displayName: "Test Bundle",
    description: "A bundle for testing",
    inputs: [
      { itemRef: { name: "item-1" } },
      { itemRef: { name: "item-2", pinnedContentHash: "0xdeadbeef1234" } },
    ],
    variables: { VAR_A: "value-a", VAR_B: "value-b" },
    ...(overrides?.spec as Record<string, unknown> ?? {}),
  },
  status: {
    phase: "Ready",
    itemCount: 2,
    resolvedHash: "abc123def456",
    ...(overrides?.status as Record<string, unknown> ?? {}),
  },
  ...Object.fromEntries(Object.entries(overrides ?? {}).filter(([key]) => !["metadata", "spec", "status"].includes(key))),
});

describe("ComposedBundleDetail", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockCanDoFn = () => true;
    mockUseControllers.mockReturnValue({
      data: [],
      isLoading: false,
      error: null,
    });
  });

  describe("Loading state", () => {
    it("shows a loading banner when the bundle is loading", () => {
      mockGetComposedBundle.mockReturnValue(new Promise(() => {}));
      renderWithProviders(<ComposedBundleDetail />);
      expect(screen.getByText("Loading bundle...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows error message when loading fails", async () => {
      mockGetComposedBundle.mockRejectedValue(new Error("Bundle not found"));
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText(/Failed to load: Bundle not found/)).toBeInTheDocument();
      });
    });

    it("shows a back link to bundles when in error state", async () => {
      mockGetComposedBundle.mockRejectedValue(new Error("not found"));
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        const backLink = screen.getByText(/Back to Bundles/);
        expect(backLink).toBeInTheDocument();
      });
    });
  });

  describe("Happy path", () => {
    it("renders the bundle name, subtitle, and health badge", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText("Test Bundle")).toBeInTheDocument();
        expect(screen.getByText(/Composed bundle · default/)).toBeInTheDocument();
        expect(screen.getByTestId("badge-Ready")).toBeInTheDocument();
      });
    });

    it("renders detail grid with Name, Namespace, Item count, JCasC strategy, Description, Created", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText("test-bundle")).toBeInTheDocument();
        expect(screen.getByText("default")).toBeInTheDocument();
        expect(screen.getByText("2")).toBeInTheDocument();
        expect(screen.getByText("merge")).toBeInTheDocument();
        expect(screen.getByText("A bundle for testing")).toBeInTheDocument();
        // Resolved hash
        expect(screen.getByText(/abc123def456/)).toBeInTheDocument();
      });
    });

    it("shows missing items banner when there are missing items", async () => {
      mockGetComposedBundle.mockResolvedValue(
        bundleFixture({ status: { phase: "Drifted", missingItems: ["item-1"], driftedItems: [] } }),
      );
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText(/Missing items:/)).toBeInTheDocument();
        expect(screen.getAllByText(/item-1/).length).toBeGreaterThanOrEqual(1);
      });
    });

    it("shows drifted items banner when there are drifted items", async () => {
      mockGetComposedBundle.mockResolvedValue(
        bundleFixture({ status: { phase: "Drifted", missingItems: [], driftedItems: ["item-2"] } }),
      );
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText(/Drifted items:/)).toBeInTheDocument();
        expect(screen.getAllByText(/item-2/).length).toBeGreaterThanOrEqual(1);
      });
    });

    it("renders the edit and delete buttons when user has permissions", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText("Edit in composer")).toBeInTheDocument();
        expect(screen.getByText("Delete")).toBeInTheDocument();
      });
    });
  });

  describe("Items section", () => {
    it("lists items in the bundle with names and pinned hashes", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText("item-1")).toBeInTheDocument();
        expect(screen.getByText("item-2")).toBeInTheDocument();
        expect(screen.getByText(/Pinned: 0xdeadbeef12/)).toBeInTheDocument();
      });
    });

    it('shows "No items in this bundle" when items are empty', async () => {
      mockGetComposedBundle.mockResolvedValue(
        bundleFixture({ spec: { inputs: [], displayName: "Empty Bundle" } }),
      );
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText(/No items in this bundle/)).toBeInTheDocument();
      });
    });
  });

  describe("Variables section", () => {
    it("renders variables when present", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText(/Variables \(2\)/)).toBeInTheDocument();
        expect(screen.getByText("VAR_A")).toBeInTheDocument();
        expect(screen.getByText("value-a")).toBeInTheDocument();
        expect(screen.getByText("VAR_B")).toBeInTheDocument();
        expect(screen.getByText("value-b")).toBeInTheDocument();
      });
    });

    it("does not render variables section when no variables exist", async () => {
      mockGetComposedBundle.mockResolvedValue(
        bundleFixture({ spec: { inputs: [{ itemRef: { name: "item-1" } }], variables: {} } }),
      );
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.queryByText(/Variables/)).not.toBeInTheDocument();
      });
    });
  });

  describe("Attach to controller", () => {
    it("shows the attach section with controller selector", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running" },
          { name: "ctrl-b", cluster: "core", namespace: "default", phase: "Connected" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText("Attach to controller")).toBeInTheDocument();
        expect(screen.getByRole("combobox")).toBeInTheDocument();
      });
    });

    it("attaches a bundle to a controller when selected and confirmed", async () => {
      mockUpdateController.mockResolvedValueOnce({});
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", phase: "Running" },
        ],
        isLoading: false,
        error: null,
      });
      const user = userEvent.setup();
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText("Attach to controller")).toBeInTheDocument();
      });

      await user.selectOptions(screen.getByRole("combobox"), "ctrl-a");
      await user.click(screen.getByRole("button", { name: "Attach" }));

      await waitFor(() => {
        expect(mockUpdateController).toHaveBeenCalledWith(
          "core", "ctrl-a", "default",
          expect.objectContaining({
            spec: { composedBundleRef: { name: "test-bundle" } },
          }),
        );
      });
    });

    it('shows "No controllers found" message when no controllers in namespace', async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-other", cluster: "core", namespace: "other-ns", phase: "Running" },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(
          screen.getByText(/No controllers found in namespace/),
        ).toBeInTheDocument();
      });
    });

    it("shows attach section only when user has controller update permission", async () => {
      mockCanDoFn = (_perms, resource, verb) =>
        resource !== "controllers" || verb !== "update";

      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.queryByText("Attach to controller")).not.toBeInTheDocument();
      });
    });
  });

  describe("Referenced by controllers", () => {
    it("shows referencing controllers when bundle is attached to controllers", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", composedBundleRef: { name: "test-bundle" } },
          { name: "ctrl-b", namespace: "default", composedBundleRef: { name: "other-bundle" } },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(screen.getByText(/Controllers \(1\)/)).toBeInTheDocument();
        expect(screen.getByText("ctrl-a", { selector: "span" })).toBeInTheDocument();
        expect(screen.queryByText("ctrl-b", { selector: "span" })).not.toBeInTheDocument();
      });
    });

    it('shows empty text when no controllers reference this bundle', async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl-a", cluster: "core", namespace: "default", composedBundleRef: { name: "other-bundle" } },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ComposedBundleDetail />);

      await waitFor(() => {
        expect(
          screen.getByText(/No controllers reference this bundle/),
        ).toBeInTheDocument();
      });
    });

    it("matches full bundle identity and builds an encoded controller route", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture({ metadata: { name: "test-bundle", namespace: "default" } }));
      mockUseControllers.mockReturnValue({
        data: [
          { name: "ctrl one", cluster: "core", namespace: "default", composedBundleRef: { name: "test-bundle" } },
          { name: "wrong-cluster", cluster: "edge", namespace: "default", composedBundleRef: { name: "test-bundle" } },
          { name: "wrong-namespace", cluster: "core", namespace: "other", composedBundleRef: { name: "test-bundle" } },
          { name: "explicit-namespace", cluster: "core", namespace: "other", composedBundleRef: { name: "test-bundle", namespace: "default" } },
        ],
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ComposedBundleDetail />);
      const link = await screen.findByRole("link", { name: /ctrl one/ });
      expect(link).toHaveAttribute("href", "/controllers/core/default/ctrl%20one");
      expect(screen.queryByText("wrong-cluster", { selector: "span" })).not.toBeInTheDocument();
      expect(screen.queryByText("wrong-namespace", { selector: "span" })).not.toBeInTheDocument();
      expect(screen.getByText("explicit-namespace", { selector: "span" })).toBeInTheDocument();
    });
  });

  it("renders the operational sections and stable resolved input identity", async () => {
    mockGetComposedBundle.mockResolvedValue(bundleFixture({
      resolvedInputs: [
        { index: 0, name: "theme", kind: "itemRef", type: "jcasc", namespace: "default", revision: "abc", status: "Resolved" },
        { index: 1, name: "jobs", kind: "itemRef", type: "item", status: "Unknown" },
      ],
    }));
    renderWithProviders(<ComposedBundleDetail />);
    for (const heading of ["Summary", "Validation", "Composition", "Impact"]) {
      expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
    }
    expect(screen.getByText("Diagnostics")).toBeInTheDocument();
    expect(screen.getByText("1. theme")).toBeInTheDocument();
    expect(screen.getByText("2. jobs")).toBeInTheDocument();
  });

  describe("Warnings and namespace badges (change F)", () => {
    it("renders a warnings banner when status.warnings is present", async () => {
      mockGetComposedBundle.mockResolvedValue(
        bundleFixture({
          status: {
            phase: "Ready",
            itemCount: 2,
            warnings: ['itemRef "theme": using default/theme; a same-named item exists in the operator namespace (varroa-system/theme) and is shadowed'],
          },
        }),
      );
      renderWithProviders(<ComposedBundleDetail />);
      await waitFor(() => {
        expect(screen.getByText(/is shadowed/)).toBeInTheDocument();
      });
    });

    it("renders no warnings banner when status.warnings is empty", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleDetail />);
      await waitFor(() => {
        expect(screen.getByText("Test Bundle")).toBeInTheDocument();
      });
      expect(screen.queryByText(/Warnings:/)).not.toBeInTheDocument();
    });

    it("shows a namespace badge on an input row whose itemRef has a namespace", async () => {
      mockGetComposedBundle.mockResolvedValue(
        bundleFixture({
          spec: {
            displayName: "Test Bundle",
            inputs: [{ itemRef: { name: "shared-theme", namespace: "team-b" } }],
          },
        }),
      );
      renderWithProviders(<ComposedBundleDetail />);
      await waitFor(() => {
        expect(screen.getByText("shared-theme")).toBeInTheDocument();
      });
      expect(screen.getByText("team-b")).toBeInTheDocument();
    });
  });
});
