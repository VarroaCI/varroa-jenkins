import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { render } from "@testing-library/react";
import { ThemeProvider } from "../context/ThemeContext";
import { ToastProvider } from "../components/Toast";
import { AuthProvider } from "../context/AuthContext";
import ComposedBundleEdit from "./ComposedBundleEdit";

vi.mock("../hooks/useConfigurationCluster", () => ({
  useConfigurationCluster: () => ({ cluster: "core", entry: null, ready: true }),
}));

// ---- Mocks ----

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
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
  getComposedBundle: (...args: unknown[]) => mockGetComposedBundle(...args),
  updateComposedBundle: vi.fn(),
  createComposedBundle: vi.fn(),
  previewComposedBundle: vi.fn(),
  validateComposedBundle: vi.fn(),
  updateController: vi.fn(),
  deleteComposedBundle: vi.fn(),
  pauseBundleRollout: vi.fn(),
  resumeBundleRollout: vi.fn(),
}));

const mockBffFetch = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  getToken: vi.fn(() => null),
  logout: vi.fn(),
}));

// Mock useControllers
const mockUseControllers = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useControllers: () => mockUseControllers(),
}));

// Mock usePermissions — use a mutable variable so tests can override canDoAnywhere
let mockCanDoFn: (...args: unknown[]) => boolean = () => true;
vi.mock("../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: { global: {}, scopes: [] } }),
  canDoAnywhere: (...args: unknown[]) => mockCanDoFn(...args),
}));

vi.mock("../components/Toast", () => ({
  useToast: () => ({ toast: vi.fn() }),
  ToastProvider: ({ children }: { children: React.ReactNode }) => children,
}));

// Mock useCatalog
vi.mock("../hooks/useCatalog", () => ({
  useCatalogItems: () => ({ data: { items: [] }, isLoading: false, error: null }),
  useCatalogSources: () => ({ data: { items: [] }, isLoading: false, error: null }),
  useComposedBundles: () => ({ data: { items: [] }, isLoading: false, error: null }),
}));

function createTestQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 0, refetchOnWindowFocus: false, refetchOnMount: false, refetchOnReconnect: false, refetchInterval: false },
      mutations: { retry: false },
    },
  });
}

function renderWithProviders(ui: React.ReactElement, route = "/catalog/bundles/default/test-bundle/edit") {
  const queryClient = createTestQueryClient();
  // Pre-seed permissions
  queryClient.setQueryData(["permissions"], { composedbundles: { update: true, get: true } });
  mockBffFetch.mockResolvedValue({});

  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <ThemeProvider>
          <ToastProvider>
            <AuthProvider>
              {/* ComposedBundleEdit provides its own bundle-scoped composer. */}
              {ui}
            </AuthProvider>
          </ToastProvider>
        </ThemeProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

const bundleFixture = (overrides?: Record<string, unknown>) => ({
  apiVersion: "varroa.dev/v1alpha1",
  kind: "ComposedBundle",
  metadata: {
    name: "test-bundle",
    namespace: "default",
    resourceVersion: "12345",
    creationTimestamp: "2024-06-01T00:00:00Z",
    ...((overrides?.metadata as Record<string, unknown>) ?? {}),
  },
  spec: {
    displayName: "Test Bundle",
    description: "A bundle for testing",
    inputs: [
      { itemRef: { name: "item-1" } },
      { itemRef: { name: "item-2", pinnedContentHash: "0xdeadbeef1234" } },
    ],
    variables: { VAR_A: "value-a" },
    jcascMergeStrategy: "override",
    ...((overrides?.spec as Record<string, unknown>) ?? {}),
  },
  status: {
    phase: "Ready",
    itemCount: 2,
    ...((overrides?.status as Record<string, unknown>) ?? {}),
  },
});

describe("ComposedBundleEdit", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockCanDoFn = (...args: unknown[]) => {
      const resource = args[0] as string;
      const verb = args[1] as string;
      if (resource === "composedbundles" && verb === "update") return true;
      return true;
    };
    mockUseControllers.mockReturnValue({ data: [], isLoading: false, error: null });
    localStorage.clear();
  });

  describe("Unauthorized", () => {
    it("redirects to bundle detail when user lacks update permission", () => {
      mockCanDoFn = () => false;
      mockGetComposedBundle.mockResolvedValue(bundleFixture());

      renderWithProviders(<ComposedBundleEdit />);

      // Should render a Navigate (which MemoryRouter handles)
      // The detail page shows a heading; since we redirected, the edit page's
      // loading/error should not appear.
      expect(screen.queryByText("Loading bundle...")).not.toBeInTheDocument();
    });
  });

  describe("Loading state", () => {
    it("shows a loading banner when the bundle is loading", () => {
      mockGetComposedBundle.mockReturnValue(new Promise(() => {}));
      renderWithProviders(<ComposedBundleEdit />);
      expect(screen.getByText("Loading bundle...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows error message when loading fails", async () => {
      mockGetComposedBundle.mockRejectedValue(new Error("Bundle not found"));
      renderWithProviders(<ComposedBundleEdit />);

      await waitFor(() => {
        expect(screen.getByText(/Failed to load: Bundle not found/)).toBeInTheDocument();
      });
    });

    it("shows a back link when bundle fails to load", async () => {
      mockGetComposedBundle.mockRejectedValue(new Error("not found"));
      renderWithProviders(<ComposedBundleEdit />);

      await waitFor(() => {
        expect(screen.getByText(/Back to Bundles/)).toBeInTheDocument();
      });
    });
  });

  describe("Seeded editor", () => {
    it("seeds composer from bundle itemRef inputs and variables", async () => {
      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleEdit />);

      // After loading, the edit banner should show
      await waitFor(() => {
        expect(screen.getByText(/Editing bundle: Test Bundle/)).toBeInTheDocument();
      });

      // The composer tray should be open (default for edit mode)
      expect(screen.getByText("Bundle Composer")).toBeInTheDocument();
    });

    it("does not render composer editor UI when bundle fails to load", async () => {
      mockGetComposedBundle.mockRejectedValue(new Error("Bundle gone"));
      renderWithProviders(<ComposedBundleEdit />);

      await waitFor(() => {
        expect(screen.getByText(/Failed to load/)).toBeInTheDocument();
      });

      expect(screen.queryByText("Bundle Composer")).not.toBeInTheDocument();
    });

    it("leaves create-draft localStorage key untouched", async () => {
      // Set up a pre-existing draft
      localStorage.setItem("varroa_composer_draft", JSON.stringify({ items: [{ name: "my-draft-item" }], variables: {} }));

      mockGetComposedBundle.mockResolvedValue(bundleFixture());
      renderWithProviders(<ComposedBundleEdit />);

      await waitFor(() => {
        expect(screen.getByText(/Editing bundle: Test Bundle/)).toBeInTheDocument();
      });

      // The edit session persists under a separate bundle-scoped key, so the
      // create-draft should remain unchanged.
      const stored = localStorage.getItem("varroa_composer_draft");
      expect(JSON.parse(stored!)).toMatchObject({ items: [{ name: "my-draft-item" }] });
    });

    it("hydrates a current-version edit draft instead of re-seeding from the bundle", async () => {
      // Route params are default/test-bundle with no ?cluster → core-scoped key.
      const editKey = "varroa_composer_edit_core_default_test-bundle";
      // Draft was seeded from the bundle's CURRENT resourceVersion (12345), so it
      // reflects the current bundle and must survive a refresh/back.
      localStorage.setItem(
        editKey,
        JSON.stringify({ items: [{ name: "draft-only-item" }], variables: {}, baseVersion: "12345" }),
      );
      // Bundle has a DIFFERENT item; if we wrongly re-seed, the in-progress draft
      // (based on the same version) is lost.
      mockGetComposedBundle.mockResolvedValue({
        ...bundleFixture(),
        spec: { ...bundleFixture().spec, inputs: [{ itemRef: { name: "bundle-item" } }] },
      });

      renderWithProviders(<ComposedBundleEdit />);

      await waitFor(() => {
        expect(screen.getByText(/Editing bundle: Test Bundle/)).toBeInTheDocument();
      });

      // The in-progress draft survived: the persisted edit draft still holds the
      // draft item, not the bundle item (no clobbering re-seed).
      const stored = JSON.parse(localStorage.getItem(editKey)!);
      expect(stored.items).toEqual([{ name: "draft-only-item" }]);
    });

    it("discards a stale (superseded-version) edit draft and re-seeds from the bundle", async () => {
      const editKey = "varroa_composer_edit_core_default_test-bundle";
      // Draft was seeded from an OLD version (1); the bundle has since moved to
      // 12345 with different inputs. The stale draft must be discarded.
      localStorage.setItem(
        editKey,
        JSON.stringify({ items: [{ name: "draft-only-item" }], variables: {}, baseVersion: "1" }),
      );
      mockGetComposedBundle.mockResolvedValue({
        ...bundleFixture(),
        spec: { ...bundleFixture().spec, inputs: [{ itemRef: { name: "bundle-item" } }] },
      });

      renderWithProviders(<ComposedBundleEdit />);

      await waitFor(() => {
        expect(screen.getByText(/Editing bundle: Test Bundle/)).toBeInTheDocument();
      });

      // Re-seeded from the current bundle: draft item gone, bundle item present,
      // and the persisted draft is re-stamped to the current version.
      await waitFor(() => {
        const stored = JSON.parse(localStorage.getItem(editKey)!);
        expect(stored.items).toEqual([{ name: "bundle-item" }]);
        expect(stored.baseVersion).toBe("12345");
      });
    });
  });

  describe("Git input passthrough", () => {
    it("passes gitSources through editTarget and does not seed them into composer", async () => {
      const bundleWithGit = bundleFixture({
        spec: {
          displayName: "Git Bundle",
          inputs: [
            { itemRef: { name: "catalog-item" } },
            { gitSource: { repoURL: "https://github.com/example/repo.git", path: "jenkins.yaml", revision: "abc123" } },
          ],
          variables: {},
        },
      });
      mockGetComposedBundle.mockResolvedValue(bundleWithGit);

      renderWithProviders(<ComposedBundleEdit />);

      await waitFor(() => {
        expect(screen.getByText(/Editing bundle: Git Bundle/)).toBeInTheDocument();
      });

      // The catalog item should be in the composer, but the git source should not.
      // Seeding lands in a separate effect after the edit banner renders, so wait
      // for the item to be committed before asserting the count.
      await waitFor(() => {
        expect(screen.getByText(/Items \(1\)/)).toBeInTheDocument();
      });
      expect(screen.getByText("catalog-item")).toBeInTheDocument();
    });
  });
});
