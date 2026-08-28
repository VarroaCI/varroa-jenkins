import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import CatalogSources from "./CatalogSources";

// Mock react-router-dom's Link since CatalogSources uses it implicitly
// (none used directly, but Toast or other components might)

const mockCreateCatalogSource = vi.fn();
const mockUpdateCatalogSource = vi.fn();
const mockDeleteCatalogSource = vi.fn();
const mockSyncCatalogSource = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
  createCatalogSource: (...args: unknown[]) => mockCreateCatalogSource(...args),
  updateCatalogSource: (...args: unknown[]) => mockUpdateCatalogSource(...args),
  deleteCatalogSource: (...args: unknown[]) => mockDeleteCatalogSource(...args),
  syncCatalogSource: (...args: unknown[]) => mockSyncCatalogSource(...args),
}));

const mockUseCatalogSources = vi.fn();
vi.mock("../hooks/useCatalog", () => ({
  useCatalogSources: () => mockUseCatalogSources(),
}));

// Mock only the data-fetching hook; canDoGlobal/canDoInNamespace stay the
// real implementations so the namespace-scoping tests below exercise actual
// capability logic, not a stub.
const mockUsePermissions = vi.fn();
vi.mock("../hooks/usePermissions", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../hooks/usePermissions")>();
  return {
    ...actual,
    usePermissions: () => mockUsePermissions(),
  };
});

const GLOBAL_ALL_PERMS = {
  global: {
    catalogsources: { create: true, update: true, delete: true, get: true, list: true },
  },
  scopes: [],
};

// Mock useToast to avoid side effects, keep ToastProvider for renderWithProviders
vi.mock("../components/Toast", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../components/Toast")>();
  return {
    ...actual,
    useToast: () => ({ toast: vi.fn() }),
  };
});

const sourceFixture = (name: string, overrides?: Record<string, unknown>) => ({
  apiVersion: "varroa.dev/v1alpha1",
  kind: "CatalogSource",
  metadata: { name, namespace: "default", creationTimestamp: new Date().toISOString() },
  spec: {
    repoURL: `https://github.com/example/${name}`,
    revision: "main",
    path: "/",
    syncIntervalSeconds: 300,
    trusted: false,
    ...(overrides?.spec as Record<string, unknown> ?? {}),
  },
  status: {
    phase: "Ready",
    itemCount: 5,
    ...(overrides?.status as Record<string, unknown> ?? {}),
  },
});

function renderPage() {
  return renderWithProviders(<CatalogSources />);
}

describe("CatalogSources", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUsePermissions.mockReturnValue({ data: GLOBAL_ALL_PERMS });
  });

  describe("Loading state", () => {
    it("shows a loading banner when sources are loading", () => {
      mockUseCatalogSources.mockReturnValue({ data: null, isLoading: true, error: null });
      renderPage();
      expect(screen.getByText("Loading catalog sources...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows an error banner when loading fails", () => {
      mockUseCatalogSources.mockReturnValue({
        data: null,
        isLoading: false,
        error: { message: "Failed to fetch sources" },
      });
      renderPage();
      expect(screen.getByText(/Failed to load: Failed to fetch sources/)).toBeInTheDocument();
    });
  });

  describe("Empty state", () => {
    it('shows "No catalog sources registered" when empty', () => {
      mockUseCatalogSources.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(
        screen.getByText(/No catalog sources registered/),
      ).toBeInTheDocument();
    });
  });

  describe("Happy path", () => {
    it("renders the page heading and description", () => {
      mockUseCatalogSources.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(screen.getByText("Catalog Sources")).toBeInTheDocument();
      expect(
        screen.getByText(/Manage repositories that provide catalog items/),
      ).toBeInTheDocument();
    });

    it('shows "New Source" button when user has create permission', () => {
      mockUseCatalogSources.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(screen.getByText("+ New Source")).toBeInTheDocument();
    });

    it("renders a table of catalog sources with all columns", () => {
      mockUseCatalogSources.mockReturnValue({
        data: {
          items: [
            sourceFixture("source-a"),
            sourceFixture("source-b", {
              spec: { repoURL: "https://github.com/example/source-b", revision: "develop", trusted: true },
              status: { phase: "Syncing", itemCount: 3 },
            }),
          ],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText("source-a")).toBeInTheDocument();
      expect(screen.getByText("source-b")).toBeInTheDocument();
      expect(screen.getByText("https://github.com/example/source-a")).toBeInTheDocument();
      expect(screen.getByText("https://github.com/example/source-b")).toBeInTheDocument();
      expect(screen.getByText("develop")).toBeInTheDocument();
      expect(screen.getByText("Ready")).toBeInTheDocument();
      expect(screen.getByText("Syncing")).toBeInTheDocument();
    });

    it("shows trusted/not-trusted indicators", () => {
      mockUseCatalogSources.mockReturnValue({
        data: {
          items: [
            sourceFixture("trusted-source", { spec: { trusted: true } }),
            sourceFixture("untrusted-source"),
          ],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      const yesLabels = screen.getAllByText("Yes");
      expect(yesLabels.length).toBeGreaterThanOrEqual(1);
      const noLabels = screen.getAllByText("No");
      expect(noLabels.length).toBeGreaterThanOrEqual(1);
    });

    it("shows Sync, Edit, and Delete buttons per source", () => {
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceFixture("source-a")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText("Sync")).toBeInTheDocument();
      expect(screen.getByText("Edit")).toBeInTheDocument();
      expect(screen.getByText("Delete")).toBeInTheDocument();
    });
  });

  describe("Create dialog", () => {
    it("opens the create dialog when clicking 'New Source'", async () => {
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceFixture("existing-source")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("+ New Source"));

      expect(screen.getByText("New Catalog Source")).toBeInTheDocument();
      expect(screen.getByPlaceholderText("https://github.com/org/repo")).toBeInTheDocument();
    });

    it("creates a source via API on form submit", async () => {
      mockCreateCatalogSource.mockResolvedValueOnce({});
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("+ New Source"));
      await user.type(
        screen.getByPlaceholderText("https://github.com/org/repo"),
        "https://github.com/my-org/my-repo",
      );
      await user.click(screen.getByRole("button", { name: "Save" }));

      await waitFor(() => {
        expect(mockCreateCatalogSource).toHaveBeenCalled();
      });
    });
  });

  describe("Edit dialog", () => {
    it("opens the edit dialog with pre-filled values", async () => {
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceFixture("source-a")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Edit"));

      expect(screen.getByText("Edit Catalog Source")).toBeInTheDocument();
      const urlInput = screen.getByDisplayValue("https://github.com/example/source-a");
      expect(urlInput).toBeInTheDocument();
    });

    it("updates a source via API on form submit", async () => {
      mockUpdateCatalogSource.mockResolvedValueOnce({});
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceFixture("source-a")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Edit"));
      await user.click(screen.getByRole("button", { name: "Save" }));

      await waitFor(() => {
        expect(mockUpdateCatalogSource).toHaveBeenCalled();
      });
    });
  });

  describe("Delete dialog", () => {
    it("opens the delete confirmation dialog", async () => {
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceFixture("source-a")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Delete"));

      expect(screen.getByText("Delete Catalog Source")).toBeInTheDocument();
      expect(screen.getByText("source-a", { selector: "b" })).toBeInTheDocument();
    });

    it("deletes a source via API when confirmed", async () => {
      mockDeleteCatalogSource.mockResolvedValueOnce(undefined);
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceFixture("source-a")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Delete"));
      await user.click(screen.getAllByRole("button", { name: "Delete" })[1]);

      await waitFor(() => {
        expect(mockDeleteCatalogSource).toHaveBeenCalledWith(
          "core", "default", "source-a",
        );
      });
    });
  });

  describe("Sync action", () => {
    it("triggers sync when Sync button is clicked", async () => {
      mockSyncCatalogSource.mockResolvedValueOnce(undefined);
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceFixture("source-a")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Sync"));

      await waitFor(() => {
        expect(mockSyncCatalogSource).toHaveBeenCalledWith("core", "default", "source-a");
      });
    });
  });

  describe("Namespace-scoped permissions", () => {
    function sourceInNamespace(name: string, namespace: string) {
      const src = sourceFixture(name);
      return { ...src, metadata: { ...src.metadata, namespace } };
    }

    it("scoped perms shape renders a <select> constrained to the scoped namespace", async () => {
      mockUsePermissions.mockReturnValue({
        data: {
          global: {},
          scopes: [
            { capabilities: { catalogsources: { create: true } }, namespaces: ["team-a"], hasControllerSelector: false },
          ],
        },
      });
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({ data: { items: [] }, isLoading: false, error: null });
      renderPage();

      await user.click(screen.getByText("+ New Source"));

      const select = screen.getByDisplayValue("team-a");
      expect(select.tagName).toBe("SELECT");
    });

    it("global perms shape renders a free-text input defaulting to varroa-system", async () => {
      mockUsePermissions.mockReturnValue({
        data: { global: { catalogsources: { create: true } }, scopes: [] },
      });
      const user = userEvent.setup();
      mockUseCatalogSources.mockReturnValue({ data: { items: [] }, isLoading: false, error: null });
      renderPage();

      await user.click(screen.getByText("+ New Source"));

      const input = screen.getByPlaceholderText("varroa-system") as HTMLInputElement;
      expect(input.tagName).toBe("INPUT");
      expect(input.value).toBe("varroa-system");
    });

    it('hides "+ New Source" when the caller has neither a global nor scoped create grant', () => {
      mockUsePermissions.mockReturnValue({ data: { global: {}, scopes: [] } });
      mockUseCatalogSources.mockReturnValue({ data: { items: [] }, isLoading: false, error: null });
      renderPage();

      expect(screen.queryByText("+ New Source")).not.toBeInTheDocument();
    });

    it("scoped user sees row actions only for sources in their own namespace", () => {
      mockUsePermissions.mockReturnValue({
        data: {
          global: {},
          scopes: [
            {
              capabilities: { catalogsources: { update: true, delete: true } },
              namespaces: ["team-a"],
              hasControllerSelector: false,
            },
          ],
        },
      });
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceInNamespace("source-a", "team-a"), sourceInNamespace("source-b", "team-b")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      // One Edit/Delete/Sync per allowed row (team-a), none for team-b.
      expect(screen.getAllByText("Edit")).toHaveLength(1);
      expect(screen.getAllByText("Delete")).toHaveLength(1);
      expect(screen.getAllByText("Sync")).toHaveLength(1);
    });

    it("a controller-selector-scoped grant must not make any namespace appear writable", () => {
      // Regression test: canDoInNamespace treats hasControllerSelector scopes as
      // permissive for any namespace, but the backend authorizer always denies
      // them for CatalogSource (controllerName is always ""). The page must not
      // leak this into "+ New Source" or any row action.
      mockUsePermissions.mockReturnValue({
        data: {
          global: {},
          scopes: [
            {
              capabilities: { catalogsources: { create: true, update: true, delete: true } },
              namespaces: [],
              hasControllerSelector: true,
            },
          ],
        },
      });
      mockUseCatalogSources.mockReturnValue({
        data: { items: [sourceInNamespace("source-a", "team-a")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.queryByText("+ New Source")).not.toBeInTheDocument();
      expect(screen.queryByText("Edit")).not.toBeInTheDocument();
      expect(screen.queryByText("Delete")).not.toBeInTheDocument();
      expect(screen.queryByText("Sync")).not.toBeInTheDocument();
    });
  });
});
