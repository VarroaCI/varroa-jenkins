import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import SettingsIndex from "./SettingsIndex";

vi.mock("../hooks/usePermissions", async () => {
  const actual = await vi.importActual<typeof import("../hooks/usePermissions")>(
    "../hooks/usePermissions",
  );
  return { ...actual, usePermissions: vi.fn() };
});

import { usePermissions } from "../hooks/usePermissions";

function renderSettingsIndex(route = "/") {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="*" element={<SettingsIndex />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("SettingsIndex", () => {
  beforeEach(() => {
    vi.mocked(usePermissions).mockReturnValue({
      data: undefined,
      isLoading: false,
      error: null,
      isError: false,
      isSuccess: false,
      isPending: true,
      status: "pending",
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 0,
      failureReason: null,
      errorUpdateCount: 0,
      isFetched: false,
      isFetchedAfterMount: false,
      isFetching: false,
      isPlaceholderData: false,
      isPaused: false,
      isRefetching: false,
      isStale: false,
      isInitialLoading: false,
      isLoadingError: false,
      isRefetchError: false,
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);
  });

  it("shows only Provisioning and Versions cards for a provisioningdefaults:update-only user", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: { provisioningdefaults: { update: true } }, scopes: [] },
      isLoading: false,
      isError: false,
      isSuccess: true,
      status: "success",
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
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderSettingsIndex();
    expect(screen.getByText("Provisioning")).toBeInTheDocument();
    expect(screen.getByText("Versions")).toBeInTheDocument();

    // Other labels should be absent
    expect(screen.queryByText("Users")).not.toBeInTheDocument();
    expect(screen.queryByText("Groups")).not.toBeInTheDocument();
    expect(screen.queryByText("Built-in Roles")).not.toBeInTheDocument();
    expect(screen.queryByText("Varroa Roles")).not.toBeInTheDocument();
    expect(screen.queryByText("Varroa Role Bindings")).not.toBeInTheDocument();
    expect(screen.queryByText("Jenkins Roles")).not.toBeInTheDocument();
    expect(screen.queryByText("Jenkins Role Bindings")).not.toBeInTheDocument();
    expect(screen.queryByText("Teams")).not.toBeInTheDocument();
    expect(screen.queryByText("Identity")).not.toBeInTheDocument();
  });

  it("shows all 12 cards for a global *:* user", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: { "*": { "*": true } }, scopes: [] },
      isLoading: false,
      isError: false,
      isSuccess: true,
      status: "success",
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
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderSettingsIndex();
    const expected = [
      "Users", "Groups", "Built-in Roles",
      "Varroa Roles", "Varroa Role Bindings",
      "Jenkins Roles", "Jenkins Role Bindings",
      "Teams", "Provisioning", "Versions", "Identity", "Update Center",
    ];
    for (const label of expected) {
      expect(screen.getByText(label)).toBeInTheDocument();
    }
  });

  it("renders NotFound when user has no admin-door permissions", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: {}, scopes: [] },
      isLoading: false,
      isError: false,
      isSuccess: true,
      status: "success",
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
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderSettingsIndex();
    // NotFoundPage renders "Not found" as the title
    expect(screen.getByText("Not found")).toBeInTheDocument();
    expect(screen.getByText("We could not find that page.")).toBeInTheDocument();
    // No card items
    expect(screen.queryByText("Provisioning")).not.toBeInTheDocument();
  });

  it("card links carry correct hrefs", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: { "*": { "*": true } }, scopes: [] },
      isLoading: false,
      isError: false,
      isSuccess: true,
      status: "success",
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
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderSettingsIndex();
    expect(screen.getByRole("link", { name: "Provisioning" })).toHaveAttribute("href", "/administration/provisioning");
    expect(screen.getByRole("link", { name: "Versions" })).toHaveAttribute("href", "/administration/versions");
    expect(screen.getByRole("link", { name: "Users" })).toHaveAttribute("href", "/access/users");
  });

  it("shows Update Center card for a global *:* user", () => {
    vi.mocked(usePermissions).mockReturnValue({
      data: { global: { "*": { "*": true } }, scopes: [] },
      isLoading: false,
      isError: false,
      isSuccess: true,
      status: "success",
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
      fetchStatus: "idle",
      promise: Promise.resolve(undefined),
      refetch: vi.fn(),
    } as unknown as ReturnType<typeof usePermissions>);

    renderSettingsIndex();
    expect(screen.getByText("Update Center")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Update Center" })).toHaveAttribute("href", "/administration/update-center");
  });
});
