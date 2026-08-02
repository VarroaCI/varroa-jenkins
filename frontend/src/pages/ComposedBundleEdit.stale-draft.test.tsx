import { StrictMode } from "react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, fireEvent, render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ThemeProvider } from "../context/ThemeContext";
import { ToastProvider } from "../components/Toast";
import { AuthProvider } from "../context/AuthContext";
import ComposedBundleEdit from "./ComposedBundleEdit";

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ namespace: "default", name: "test-bundle" }),
    useNavigate: () => vi.fn(),
  };
});

const mockGetComposedBundle = vi.fn();
const mockUpdateComposedBundle = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }, { name: "hive", core: false, healthy: true, state: "active" }]),
  getComposedBundle: (...a: unknown[]) => mockGetComposedBundle(...a),
  updateComposedBundle: (...a: unknown[]) => mockUpdateComposedBundle(...a),
  createComposedBundle: vi.fn(),
  previewComposedBundle: vi.fn(),
  validateComposedBundle: vi.fn(),
  updateController: vi.fn(),
  deleteComposedBundle: vi.fn(),
  pauseBundleRollout: vi.fn(),
  resumeBundleRollout: vi.fn(),
}));

vi.mock("../hooks/useApi", () => ({
  bffFetch: vi.fn().mockResolvedValue({}),
  getToken: vi.fn(() => null),
  logout: vi.fn(),
}));

vi.mock("../hooks/useControllers", () => ({
  useControllers: () => ({ data: [], isLoading: false, error: null }),
}));

vi.mock("../hooks/usePermissions", () => ({
  usePermissions: () => ({ data: { global: {}, scopes: [] } }),
  canDoAnywhere: () => true,
}));

vi.mock("../components/Toast", () => ({
  useToast: () => ({ toast: vi.fn() }),
  ToastProvider: ({ children }: { children: React.ReactNode }) => children,
}));

vi.mock("../hooks/useCatalog", () => ({
  useCatalogItems: () => ({
    data: {
      items: [
        {
          name: "platform-catalog-plugins-theme",
          namespace: "default",
          type: "plugin",
          displayName: "Theme",
          sourceRef: "src",
          valid: true,
        },
      ],
    },
    isLoading: false,
    error: null,
  }),
  useCatalogSources: () => ({ data: { items: [] }, isLoading: false, error: null }),
  useComposedBundles: () => ({ data: { items: [] }, isLoading: false, error: null }),
}));

// Server bundle: two items, resourceVersion 999 (the CURRENT truth).
const currentBundle = () => ({
  apiVersion: "varroa.dev/v1alpha1",
  kind: "ComposedBundle",
  metadata: { name: "test-bundle", namespace: "default", resourceVersion: "999" },
  spec: {
    displayName: "Test Bundle",
    inputs: [{ itemRef: { name: "item-1" } }, { itemRef: { name: "item-2" } }],
    variables: {},
    jcascMergeStrategy: "override",
  },
  status: { phase: "Ready", itemCount: 2 },
});

// No ?cluster in the route → the draft key folds in the default "core" cluster.
const EDIT_KEY = "varroa_composer_edit_core_default_test-bundle";

function renderPage(route = "/catalog/bundles/default/test-bundle/edit") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <StrictMode>
      <MemoryRouter initialEntries={[route]}>
        <QueryClientProvider client={qc}>
          <ThemeProvider>
            <ToastProvider>
              <AuthProvider>
                <ComposedBundleEdit />
              </AuthProvider>
            </ToastProvider>
          </ThemeProvider>
        </QueryClientProvider>
      </MemoryRouter>
    </StrictMode>,
  );
}

describe("#261: stale localStorage edit-draft must not desync the save", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    localStorage.clear();
    mockUpdateComposedBundle.mockResolvedValue({});
    mockGetComposedBundle.mockResolvedValue(currentBundle());
  });

  it("discards a draft based on a superseded bundle version and seeds from the server", async () => {
    // A leftover draft from a prior session, based on an OLD bundle version (7),
    // whose inputs no longer match the server (it drifted while the user was
    // debugging via kubectl). It even carries a bogus item the bundle never had.
    localStorage.setItem(
      EDIT_KEY,
      JSON.stringify({
        items: [{ name: "item-1" }, { name: "stale-only" }],
        variables: {},
        baseVersion: "7",
      }),
    );

    renderPage();

    // The editor must reflect the CURRENT server bundle (item-1, item-2), not the
    // stale draft (which had item-1 + stale-only). Wait on item-2 specifically:
    // both the stale draft and the re-seeded bundle have 2 items, so the "Items
    // (2)" count is ambiguous and races the re-seed.
    await waitFor(() => expect(screen.getByText("item-2")).toBeInTheDocument());
    expect(screen.queryByText("stale-only")).not.toBeInTheDocument();

    // Add the theme plugin and save.
    fireEvent.click(screen.getByText("+ Add to bundle"));
    await waitFor(() => expect(screen.getByText(/Items \(3\)/)).toBeInTheDocument());
    fireEvent.click(screen.getByText("Save changes"));

    await waitFor(() => expect(mockUpdateComposedBundle).toHaveBeenCalled());
    const [, , , body] = mockUpdateComposedBundle.mock.calls[0];
    const names = (body.spec.inputs ?? []).map((i: { itemRef?: { name: string } }) => i.itemRef?.name);
    expect(names).toEqual(["item-1", "item-2", "platform-catalog-plugins-theme"]);
  });

  it("keeps per-cluster edit drafts isolated and re-resolves against the switched cluster (#305)", async () => {
    // A leftover CORE draft (bogus item) must NOT bleed into a hive-cluster edit
    // session: the draft key folds in the cluster, so switching clusters (a new
    // ?cluster=) targets a different key and forces a fresh re-seed/re-resolve
    // against the switched cluster's bundle + catalog.
    localStorage.setItem(
      "varroa_composer_edit_core_default_test-bundle",
      JSON.stringify({ items: [{ name: "core-only-stale" }], variables: {}, baseVersion: "999", cluster: "core" }),
    );

    renderPage("/catalog/bundles/default/test-bundle/edit?cluster=hive");

    // Bundle load is addressed to the hive cluster, not the core.
    await waitFor(() =>
      expect(mockGetComposedBundle).toHaveBeenCalledWith("hive", "default", "test-bundle"),
    );

    // The editor reflects the hive bundle (item-1, item-2); the core draft's
    // bogus item never appears.
    await waitFor(() => expect(screen.getByText("item-2")).toBeInTheDocument());
    expect(screen.queryByText("core-only-stale")).not.toBeInTheDocument();

    // The hive session persisted its draft under a cluster-scoped key, leaving
    // the core draft untouched.
    await waitFor(() => {
      const hiveDraft = JSON.parse(
        localStorage.getItem("varroa_composer_edit_hive_default_test-bundle")!,
      );
      expect(hiveDraft.cluster).toBe("hive");
    });
    const coreDraft = JSON.parse(localStorage.getItem("varroa_composer_edit_core_default_test-bundle")!);
    expect(coreDraft.items).toEqual([{ name: "core-only-stale" }]);
  });
});
