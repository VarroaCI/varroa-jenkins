import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import ComposedBundles from "./ComposedBundles";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false } },
});

function renderWithProviders(ui: React.ReactElement) {
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockDeleteComposedBundle = vi.fn();
vi.mock("../api/client", () => ({
  listClusters: () => Promise.resolve([{ name: "core", core: true, healthy: true, state: "active" }]),
  deleteComposedBundle: (...args: unknown[]) => mockDeleteComposedBundle(...args),
}));

const mockUseComposedBundles = vi.fn();
vi.mock("../hooks/useCatalog", () => ({
  useComposedBundles: () => mockUseComposedBundles(),
}));

vi.mock("../hooks/usePermissions", () => ({
  usePermissions: () => ({
    data: { global: { composedbundles: { delete: true, get: true, list: true } }, scopes: [] },
  }),
  canDoAnywhere: () => true,
}));

vi.mock("../components/Toast", () => ({
  useToast: () => ({ toast: vi.fn() }),
}));

const bundleFixture = (name: string, overrides?: Record<string, unknown>) => ({
  apiVersion: "varroa.dev/v1alpha1",
  kind: "ComposedBundle",
  metadata: { name, namespace: "default" },
  spec: {
    displayName: `Display ${name}`,
    items: [{ name: "item-1" }, { name: "item-2" }],
    ...(overrides?.spec as Record<string, unknown> ?? {}),
  },
  status: {
    phase: "Ready",
    itemCount: 2,
    resolvedHash: "0xabc123def456789",
    ...(overrides?.status as Record<string, unknown> ?? {}),
  },
});

function renderPage() {
  return renderWithProviders(<ComposedBundles />);
}

describe("ComposedBundles", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe("Loading state", () => {
    it("shows a loading banner when bundles are loading", () => {
      mockUseComposedBundles.mockReturnValue({ data: null, isLoading: true, error: null });
      renderPage();
      expect(screen.getByText("Loading composed bundles...")).toBeInTheDocument();
    });
  });

  describe("Error state", () => {
    it("shows an error banner when loading fails", () => {
      mockUseComposedBundles.mockReturnValue({
        data: null,
        isLoading: false,
        error: { message: "API error" },
      });
      renderPage();
      expect(screen.getByText(/Failed to load: API error/)).toBeInTheDocument();
    });
  });

  describe("Empty state", () => {
    it('shows "No composed bundles yet" when empty', () => {
      mockUseComposedBundles.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(
        screen.getByText(/No composed bundles yet/),
      ).toBeInTheDocument();
    });
  });

  describe("Happy path", () => {
    it("renders the page heading and description", () => {
      mockUseComposedBundles.mockReturnValue({
        data: { items: [] },
        isLoading: false,
        error: null,
      });
      renderPage();
      expect(screen.getByText("Composed Bundles")).toBeInTheDocument();
      expect(
        screen.getByText(/Compositions assembled from catalog items/),
      ).toBeInTheDocument();
    });

    it("renders a table of bundles with Name, Items, Phase, Resolved Hash, Missing, Drifted", () => {
      mockUseComposedBundles.mockReturnValue({
        data: {
          items: [
            bundleFixture("bundle-a"),
            bundleFixture("bundle-b", {
              status: {
                phase: "Drifted",
                itemCount: 3,
                resolvedHash: "0xdeadbeef",
                missingItems: ["item-x"],
                driftedItems: ["item-y"],
              },
            }),
          ],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText("Display bundle-a")).toBeInTheDocument();
      expect(screen.getByText("Display bundle-b")).toBeInTheDocument();
      // Phases
      expect(screen.getByText("Ready")).toBeInTheDocument();
      const driftedPills = screen.getAllByText("Drifted");
      expect(driftedPills.length).toBeGreaterThanOrEqual(1);
      // Hashes
      expect(screen.getByText("0xabc123def4...")).toBeInTheDocument();
      expect(screen.getByText("0xdeadbeef...")).toBeInTheDocument();
      // Missing/drifted counts — badges show numbers
      const ones = screen.getAllByText("1");
      expect(ones.length).toBeGreaterThanOrEqual(2);
    });

    it("shows checkmarks when no missing or drifted items", () => {
      mockUseComposedBundles.mockReturnValue({
        data: {
          items: [
            bundleFixture("bundle-a"),
          ],
        },
        isLoading: false,
        error: null,
      });
      renderPage();

      const checkmarks = screen.getAllByText("✓");
      expect(checkmarks.length).toBeGreaterThanOrEqual(2);
    });
  });

  describe("Row click and navigation", () => {
    it("navigates to bundle detail on row click", async () => {
      const user = userEvent.setup();
      mockUseComposedBundles.mockReturnValue({
        data: { items: [bundleFixture("my-bundle")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Display my-bundle"));

      expect(mockNavigate).toHaveBeenCalledWith(
        "/catalog/bundles/default/my-bundle?cluster=core",
      );
    });
  });

  describe("Delete action", () => {
    it("shows Delete button per bundle", () => {
      mockUseComposedBundles.mockReturnValue({
        data: { items: [bundleFixture("my-bundle")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      expect(screen.getByText("Delete")).toBeInTheDocument();
    });

    it("deletes a bundle when Delete is clicked", async () => {
      mockDeleteComposedBundle.mockResolvedValueOnce(undefined);
      const user = userEvent.setup();
      mockUseComposedBundles.mockReturnValue({
        data: { items: [bundleFixture("my-bundle")] },
        isLoading: false,
        error: null,
      });
      renderPage();

      await user.click(screen.getByText("Delete"));

      await waitFor(() => {
        expect(mockDeleteComposedBundle).toHaveBeenCalledWith("core", "default", "my-bundle");
      });
    });
  });
});
