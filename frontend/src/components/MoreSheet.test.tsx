import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { MoreSheet } from "./MoreSheet";

vi.mock("../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../hooks/usePermissions")>(
    "../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn() };
});

import { usePermissions } from "../hooks/usePermissions";

const fullPermissionsFixture = {
  data: { global: { "*": { "*": true } }, scopes: [] },
  isLoading: false,
  isError: false,
  isSuccess: true,
  status: "success" as const,
  isPending: false,
  dataUpdatedAt: 0,
  errorUpdatedAt: 0,
  failureCount: 0,
  failureReason: null,
  errorUpdateCount: 0,
  isFetched: true,
  isFetchedAfterMount: true,
  isFetching: false,
  isPlaceholderData: false,
  isPaused: false,
  isRefetching: false,
  isStale: false,
  isInitialLoading: false,
  isLoadingError: false,
  isRefetchError: false,
  fetchStatus: "idle" as const,
  promise: Promise.resolve(undefined),
  refetch: vi.fn(),
};

function renderMoreSheet(route = "/") {
  const queryClient = createTestQueryClient();
  const onClose = vi.fn();
  const result = render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <MoreSheet onClose={onClose} />
      </QueryClientProvider>
    </MemoryRouter>,
  );
  return { ...result, onClose };
}

describe("MoreSheet", () => {
  beforeEach(() => {
    vi.mocked(usePermissions).mockReturnValue(
      fullPermissionsFixture as unknown as ReturnType<typeof usePermissions>,
    );
  });

  afterEach(() => {
    document.body.style.overflow = "";
  });

  it("renders dialog with aria-modal and aria-label", () => {
    renderMoreSheet("/");
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAttribute("aria-label", "More navigation");
  });

  it("renders all four groups with full permissions", () => {
    renderMoreSheet("/");
    // Group labels
    expect(screen.getByText("Operate")).toBeInTheDocument();
    expect(screen.getByText("Brood")).toBeInTheDocument();
    expect(screen.getByText("Manage")).toBeInTheDocument();
    expect(screen.getByText("Account")).toBeInTheDocument();

    // Operate items
    expect(screen.getByRole("link", { name: "Plugins" })).toHaveAttribute("href", "/plugins");

    // Brood items
    expect(screen.getByRole("link", { name: "Operations" })).toHaveAttribute("href", "/brood-operations");
    expect(screen.getByRole("link", { name: "Schedules" })).toHaveAttribute("href", "/brood-schedules");

    // Manage items
    expect(screen.getByRole("link", { name: "Catalog" })).toHaveAttribute("href", "/catalog");
    expect(screen.getByRole("link", { name: "Admin & access" })).toHaveAttribute("href", "/settings");

    // Account items
    expect(screen.getByRole("link", { name: "My profile" })).toHaveAttribute("href", "/profile");
  });

  it("hides the Manage group when neither catalog nor admin permissions are held", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: {}, scopes: [] },
      isLoading: false,
      isError: false,
      isSuccess: true,
      status: "success" as const,
      isPending: false,
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 0,
      failureReason: null,
      errorUpdateCount: 0,
      isFetched: true,
      isFetchedAfterMount: true,
      isFetching: false,
      isPlaceholderData: false,
      isPaused: false,
      isRefetching: false,
      isStale: false,
      isInitialLoading: false,
      isLoadingError: false,
      isRefetchError: false,
      fetchStatus: "idle" as const,
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderMoreSheet("/");
    expect(screen.queryByText("Manage")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Catalog" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Admin & access" })).not.toBeInTheDocument();
    // Operate group also hidden when controllers:read not held
    expect(screen.queryByText("Operate")).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Plugins" })).not.toBeInTheDocument();
    // Brood and Account still there
    expect(screen.getByText("Brood")).toBeInTheDocument();
    expect(screen.getByText("Account")).toBeInTheDocument();
  });

  it("shows only Catalog in Manage when only catalogitems:read is held (namespace-scoped)", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: {
        global: {},
        scopes: [
          {
            namespaces: ["team-a"],
            hasControllerSelector: false,
            capabilities: { catalogitems: { read: true } },
          },
        ],
      },
      isLoading: false,
      isError: false,
      isSuccess: true,
      status: "success" as const,
      isPending: false,
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 0,
      failureReason: null,
      errorUpdateCount: 0,
      isFetched: true,
      isFetchedAfterMount: true,
      isFetching: false,
      isPlaceholderData: false,
      isPaused: false,
      isRefetching: false,
      isStale: false,
      isInitialLoading: false,
      isLoadingError: false,
      isRefetchError: false,
      fetchStatus: "idle" as const,
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderMoreSheet("/");
    expect(screen.getByText("Manage")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Catalog" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Admin & access" })).not.toBeInTheDocument();
  });

  it("calls onClose when a link is clicked", () => {
    const { onClose } = renderMoreSheet("/");
    fireEvent.click(screen.getByRole("link", { name: "Operations" }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it("calls onClose when the scrim is clicked", () => {
    const { onClose } = renderMoreSheet("/");
    const scrim = screen.getByRole("dialog").previousElementSibling;
    expect(scrim).toBeInTheDocument();
    fireEvent.click(scrim!);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  describe("focus and dismissal", () => {
    it("focuses the first link on mount", () => {
      renderMoreSheet("/");
      const firstLink = screen.getByRole("link", { name: "Plugins" });
      expect(document.activeElement).toBe(firstLink);
    });

    it("calls onClose on Escape key", () => {
      const { onClose } = renderMoreSheet("/");
      const dialog = screen.getByRole("dialog");
      fireEvent.keyDown(dialog, { key: "Escape" });
      expect(onClose).toHaveBeenCalledTimes(1);
    });

    it("locks body scroll while mounted and restores on unmount", () => {
      const { unmount } = renderMoreSheet("/");
      expect(document.body.style.overflow).toBe("hidden");

      unmount();
      expect(document.body.style.overflow).toBe("");
    });

    it("wraps Tab from the last link to the first", () => {
      renderMoreSheet("/");
      const links = screen.getAllByRole("link");
      const first = links[0];
      const last = links[links.length - 1];

      // Focus last link, press Tab → focus wraps to first
      last.focus();
      fireEvent.keyDown(last, { key: "Tab" });
      expect(document.activeElement).toBe(first);

      // Focus first link, press Shift+Tab → focus wraps to last
      first.focus();
      fireEvent.keyDown(first, { key: "Tab", shiftKey: true });
      expect(document.activeElement).toBe(last);
    });
  });
});
