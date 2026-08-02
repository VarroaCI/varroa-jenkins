import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BundleSelector, BundleHealthBadge } from "./BundleSelector";
import type { ComposedBundleList } from "../types";

// Mock useComposedBundles hook
vi.mock("../hooks/useCatalog", () => ({
  useComposedBundles: vi.fn(),
}));

import { useComposedBundles } from "../hooks/useCatalog";

const mockBundles = (items: unknown[]) => {
  vi.mocked(useComposedBundles).mockReturnValue({
    data: { items, apiVersion: "varroa.dev/v1alpha1", kind: "ComposedBundleList" },
    isLoading: false,
    error: null,
    isError: false,
    isInitialLoading: false,
    isEnabled: true,
    isPending: false,
    isLoadingError: false,
    isRefetchError: false,
    isSuccess: true,
    status: "success",
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
    refetch: vi.fn(),
    fetchStatus: "idle",
    promise: Promise.resolve({ items, apiVersion: "varroa.dev/v1alpha1", kind: "ComposedBundleList" }),
  } as unknown as ReturnType<typeof useComposedBundles>);
};

describe("BundleHealthBadge", () => {
  it("renders Ready badge", () => {
    render(<BundleHealthBadge phase="Ready" />);
    expect(screen.getByText("Ready")).toBeInTheDocument();
  });

  it("renders Invalid badge", () => {
    render(<BundleHealthBadge phase="Invalid" />);
    expect(screen.getByText("Invalid")).toBeInTheDocument();
  });

  it("renders Drifted badge", () => {
    render(<BundleHealthBadge phase="Drifted" />);
    expect(screen.getByText("Drifted")).toBeInTheDocument();
  });

  it("renders Pending badge", () => {
    render(<BundleHealthBadge phase="Pending" />);
    expect(screen.getByText("Pending")).toBeInTheDocument();
  });

  it("renders nothing when phase is undefined", () => {
    const { container } = render(<BundleHealthBadge phase={undefined} />);
    expect(container.firstChild).toBeNull();
  });
});

describe("BundleSelector", () => {
  const baseProps = { cluster: "core", namespace: "my-ns", value: null as string | null, onChange: vi.fn() };

  it("lists bundles in the select dropdown", () => {
    mockBundles([
      { metadata: { name: "bundle-a" }, spec: { displayName: "Bundle A" } },
      { metadata: { name: "bundle-b" }, spec: {} },
    ]);
    render(<BundleSelector {...baseProps} />);
    expect(screen.getByText("Bundle A")).toBeInTheDocument();
    expect(screen.getByText("bundle-b")).toBeInTheDocument();
  });

  it("reports selection on change", async () => {
    const onChange = vi.fn();
    mockBundles([
      { metadata: { name: "bundle-a" }, spec: { displayName: "Bundle A" } },
    ]);
    render(<BundleSelector {...baseProps} onChange={onChange} />);
    await userEvent.selectOptions(screen.getByRole("combobox"), "bundle-a");
    expect(onChange).toHaveBeenCalledWith("bundle-a");
  });

  it("allows clearing by selecting empty option", async () => {
    const onChange = vi.fn();
    mockBundles([
      { metadata: { name: "bundle-a" }, spec: {} },
    ]);
    render(<BundleSelector {...baseProps} value="bundle-a" onChange={onChange} />);
    await userEvent.selectOptions(screen.getByRole("combobox"), "");
    expect(onChange).toHaveBeenCalledWith(null);
  });

  it("allows clearing via clear button", async () => {
    const onChange = vi.fn();
    mockBundles([
      { metadata: { name: "bundle-a" }, spec: {} },
    ]);
    render(<BundleSelector {...baseProps} value="bundle-a" onChange={onChange} />);
    await userEvent.click(screen.getByTitle("Clear selection"));
    expect(onChange).toHaveBeenCalledWith(null);
  });

  it("reflects controlled value as selected option", () => {
    mockBundles([
      { metadata: { name: "bundle-a" }, spec: {} },
      { metadata: { name: "bundle-b" }, spec: {} },
    ]);
    render(<BundleSelector {...baseProps} value="bundle-a" />);
    const select = screen.getByRole("combobox") as HTMLSelectElement;
    expect(select.value).toBe("bundle-a");
  });

  it("renders health badge for selected bundle", () => {
    mockBundles([
      {
        metadata: { name: "bundle-a" },
        spec: {},
        status: { phase: "Ready" as const },
      },
    ]);
    render(<BundleSelector {...baseProps} value="bundle-a" />);
    expect(screen.getByText("Ready")).toBeInTheDocument();
  });

  it("renders errors for a non-Ready selected bundle", () => {
    mockBundles([
      {
        metadata: { name: "bundle-bad" },
        spec: {},
        status: {
          phase: "Invalid" as const,
          errors: ["Missing required item: base-jcasc"],
          warnings: ["Consider pinning versions"],
        },
      },
    ]);
    render(<BundleSelector {...baseProps} value="bundle-bad" />);
    expect(screen.getByText(/Missing required item/)).toBeInTheDocument();
    expect(screen.getByText(/Consider pinning versions/)).toBeInTheDocument();
  });

  it("shows loading state", () => {
    vi.mocked(useComposedBundles).mockReturnValue({
      data: undefined,
      isLoading: true,
      error: null,
      isError: false,
      isInitialLoading: true,
      isEnabled: true,
      isPending: true,
      isLoadingError: false,
      isRefetchError: false,
      isSuccess: false,
      status: "pending",
      dataUpdatedAt: 0,
      errorUpdatedAt: 0,
      failureCount: 0,
      failureReason: null,
      errorUpdateCount: 0,
      isFetched: false,
      isFetchedAfterMount: false,
      isFetching: true,
      isPlaceholderData: false,
      isPaused: false,
      isRefetching: false,
      isStale: false,
      refetch: vi.fn(),
      fetchStatus: "fetching",
      promise: Promise.resolve(undefined as unknown as ComposedBundleList),
    } as unknown as ReturnType<typeof useComposedBundles>);
    render(<BundleSelector {...baseProps} />);
    expect(screen.getByText("Loading bundles...")).toBeInTheDocument();
  });
});
