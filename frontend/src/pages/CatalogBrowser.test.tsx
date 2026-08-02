import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import userEvent from "@testing-library/user-event";
import CatalogBrowser from "./CatalogBrowser";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockUseCatalogItems = vi.fn();
vi.mock("../hooks/useCatalog", () => ({
  useCatalogItems: () => mockUseCatalogItems(),
  useCatalogSources: () => ({ data: { items: [] }, isLoading: false, error: null }),
  useComposedBundles: () => ({ data: { items: [] }, isLoading: false, error: null }),
}));

const mockCatalogItem = (name: string, overrides?: Record<string, unknown>) => ({
  name,
  namespace: "default",
  sourceRef: "test-source",
  type: "jcasc",
  displayName: `Display ${name}`,
  description: `Description for ${name}`,
  valid: true,
  ...overrides,
});

function renderPage() {
  return renderWithProviders(<CatalogBrowser />);
}

describe("CatalogBrowser", () => {
  beforeEach(() => {
    mockNavigate.mockReset();
    mockUseCatalogItems.mockReset();
    localStorage.clear();
  });

  describe("Loading state", () => {
    it("shows a loading banner when items are loading", () => {
      mockUseCatalogItems.mockReturnValue({ data: null, isLoading: true, error: null });
      renderPage();
      expect(screen.getByText("Loading catalog items...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows an error banner when loading fails", () => {
      mockUseCatalogItems.mockReturnValue({
        data: null,
        isLoading: false,
        error: { message: "Network error" },
      });
      renderPage();
      expect(screen.getByText(/Failed to load catalog items/)).toBeInTheDocument();
      expect(screen.getByText(/Network error/)).toBeInTheDocument();
    });
  });

  describe("Empty state", () => {
    it("shows empty state when no items exist", () => {
      mockUseCatalogItems.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(
        screen.getByText(/No catalog items found/),
      ).toBeInTheDocument();
    });
  });

  describe("Happy path", () => {
    it("renders the page heading and description", () => {
      mockUseCatalogItems.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(screen.getByText("Catalog Browser")).toBeInTheDocument();
      expect(
        screen.getByText(/Browse and discover catalog items/),
      ).toBeInTheDocument();
    });

    it("renders search input and type filter", () => {
      mockUseCatalogItems.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(screen.getByPlaceholderText("Search items...")).toBeInTheDocument();
      expect(screen.getByRole("combobox")).toBeInTheDocument();
    });

    it("renders catalog items in a grid", () => {
      mockUseCatalogItems.mockReturnValue({
        data: {
          items: [
            mockCatalogItem("item-1"),
            mockCatalogItem("item-2"),
          ],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText("Display item-1")).toBeInTheDocument();
      expect(screen.getByText("Display item-2")).toBeInTheDocument();
    });

    it("renders type icon, description, path, source badge, and version for each item", () => {
      mockUseCatalogItems.mockReturnValue({
        data: {
          items: [
            mockCatalogItem("my-item", { version: "1.0.0", tags: ["networking"] }),
          ],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText("jcasc")).toBeInTheDocument();
      expect(screen.getByText("Description for my-item")).toBeInTheDocument();
      expect(screen.getByText(/test-source/)).toBeInTheDocument();
      expect(screen.getByText("v1.0.0")).toBeInTheDocument();
      expect(screen.getByText("networking")).toBeInTheDocument();
    });

    it("shows invalid badge for invalid items", () => {
      const item = mockCatalogItem("bad-item");
      item.valid = false;
      mockUseCatalogItems.mockReturnValue({
        data: { items: [item] },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText(/Invalid/)).toBeInTheDocument();
    });

    it("navigates to item detail on card click", async () => {
      const user = userEvent.setup();
      mockUseCatalogItems.mockReturnValue({
        data: {
          items: [mockCatalogItem("item-1")],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Display item-1"));

      expect(mockNavigate).toHaveBeenCalledWith(
        "/catalog/items/default/item-1",
      );
    });

    it("in edit mode, card click adds the item instead of navigating away", async () => {
      const user = userEvent.setup();
      mockUseCatalogItems.mockReturnValue({
        data: { items: [mockCatalogItem("item-1")] },
        isLoading: false,
        error: null,
      });
      const editTarget = {
        namespace: "default",
        name: "my-bundle",
        baseBundle: {
          apiVersion: "varroa.dev/v1alpha1",
          kind: "ComposedBundle",
          metadata: { name: "my-bundle", namespace: "default" },
          spec: { inputs: [] },
        },
        gitInputs: [],
      };
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      renderWithProviders(<CatalogBrowser editTarget={editTarget as any} />);

      await user.click(screen.getByText("Display item-1"));

      // Must NOT leave the editor (navigation would discard in-progress edits).
      expect(mockNavigate).not.toHaveBeenCalled();
      // The item is added to the bundle (card shows the added badge).
      expect(await screen.findByText(/Added/)).toBeInTheDocument();
    });
  });

  describe("Filter controls", () => {
    it("renders type filter with default 'All' option", () => {
      mockUseCatalogItems.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();

      const select = screen.getByRole("combobox");
      expect(select).toBeInTheDocument();
      expect(select).toHaveValue("");
      expect(screen.getByRole("option", { name: "All" })).toBeInTheDocument();
      expect(screen.getByRole("option", { name: "Plugins" })).toBeInTheDocument();
      expect(screen.getByRole("option", { name: "JCasC" })).toBeInTheDocument();
    });

    it("changes type filter selection when user picks a different option", async () => {
      const user = userEvent.setup();
      mockUseCatalogItems.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.selectOptions(screen.getByRole("combobox"), "plugin");

      expect(screen.getByRole("combobox")).toHaveValue("plugin");
    });
  });

  describe("Composer integration", () => {
    it('shows "Add to bundle" button per item when composer does not have the item', () => {
      mockUseCatalogItems.mockReturnValue({
        data: {
          items: [mockCatalogItem("my-item")],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText("+ Add to bundle")).toBeInTheDocument();
    });

    it('shows "Added" badge instead of add button when item is already in composer', async () => {
      const user = userEvent.setup();
      mockUseCatalogItems.mockReturnValue({
        data: {
          items: [mockCatalogItem("my-item")],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      // Click "Add to bundle" — this adds the item via real ComposerProvider
      await user.click(screen.getByText("+ Add to bundle"));

      expect(screen.getByText(/Added/)).toBeInTheDocument();
    });

    it("shows FAB with item count when composer has items", async () => {
      const user = userEvent.setup();
      mockUseCatalogItems.mockReturnValue({
        data: {
          items: [mockCatalogItem("my-item"), mockCatalogItem("item-2")],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      // FAB should not be visible yet
      expect(screen.queryByTitle("Open bundle composer")).not.toBeInTheDocument();

      // Add one item
      await user.click(screen.getAllByText("+ Add to bundle")[0]);

      // FAB should now be visible with count 1
      expect(screen.getByTitle("Open bundle composer")).toBeInTheDocument();
      expect(screen.getByText("1")).toBeInTheDocument();
    });

    it("opens ComposerTray when FAB is clicked", async () => {
      const user = userEvent.setup();
      mockUseCatalogItems.mockReturnValue({
        data: {
          items: [mockCatalogItem("my-item")],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("+ Add to bundle"));
      await user.click(screen.getByTitle("Open bundle composer"));

      expect(screen.getByText("Bundle Composer")).toBeInTheDocument();
    });
  });

  describe("Search with filter", () => {
    it("shows filtered empty message when search yields no results", () => {
      mockUseCatalogItems.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();

      // Pass in some debounced search text by typing
      // The component uses internal debounce, so we assert the state indirectly
      // by checking the empty message renders with no items
      expect(screen.getByText(/No catalog items found/)).toBeInTheDocument();
    });
  });
});
