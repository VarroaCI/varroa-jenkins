import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { useCatalogSources, useCatalogItems, useComposedBundles } from "./useCatalog";
import {
  createCatalogSource,
  createCatalogItemSummary,
  createComposedBundle,
} from "../test/factories";
import type { CatalogSourceList, CatalogItemList, ComposedBundleList } from "../types";

// Mock the API client functions used by the catalog hooks.
const mockListCatalogSources = vi.fn();
const mockListCatalogItems = vi.fn();
const mockListComposedBundles = vi.fn();

vi.mock("../api/client", () => ({
  listCatalogSources: (...args: unknown[]) => mockListCatalogSources(...args),
  listCatalogItems: (...args: unknown[]) => mockListCatalogItems(...args),
  listComposedBundles: (...args: unknown[]) => mockListComposedBundles(...args),
}));

function createWrapper(queryClient = createTestQueryClient()) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  mockListCatalogSources.mockReset();
  mockListCatalogItems.mockReset();
  mockListComposedBundles.mockReset();
});

describe("useCatalogSources", () => {
  it("does not request cluster-scoped data without an accessible cluster", () => {
    const { result } = renderHook(() => useCatalogSources(null), { wrapper: createWrapper() });
    expect(result.current.fetchStatus).toBe("idle");
    expect(mockListCatalogSources).not.toHaveBeenCalled();
  });

  it("calls listCatalogSources with no args and has queryKey ['catalog-sources', undefined]", async () => {
    const qc = createTestQueryClient();
    const sourceList: CatalogSourceList = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "CatalogSourceList",
      items: [createCatalogSource()],
    };
    mockListCatalogSources.mockResolvedValueOnce(sourceList);

    const { result } = renderHook(() => useCatalogSources("core"), {
      wrapper: createWrapper(qc),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(mockListCatalogSources).toHaveBeenCalledWith("core", undefined);
    expect(result.current.data).toEqual(sourceList);
    expect(qc.getQueryData(["catalog-sources", "core", undefined])).toEqual(sourceList);
  });

  it("includes namespace in queryKey and passes it to listCatalogSources", async () => {
    const qc = createTestQueryClient();
    const sourceList: CatalogSourceList = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "CatalogSourceList",
      items: [createCatalogSource()],
    };
    mockListCatalogSources.mockResolvedValueOnce(sourceList);

    const { result } = renderHook(() => useCatalogSources("core", "my-ns"), {
      wrapper: createWrapper(qc),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(mockListCatalogSources).toHaveBeenCalledWith("core", "my-ns");
    expect(qc.getQueryData(["catalog-sources", "core", "my-ns"])).toEqual(sourceList);
  });
});

describe("useCatalogItems", () => {
  it("is disabled without an accessible cluster", () => {
    const { result } = renderHook(() => useCatalogItems(null, {}), { wrapper: createWrapper() });
    expect(result.current.fetchStatus).toBe("idle");
    expect(mockListCatalogItems).not.toHaveBeenCalled();
  });

  it("builds correct queryKey from params", async () => {
    const qc = createTestQueryClient();
    const itemList: CatalogItemList = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "CatalogItemList",
      items: [createCatalogItemSummary()],
    };
    mockListCatalogItems.mockResolvedValueOnce(itemList);

    const params = { namespace: "ns", source: "src", type: "jcasc", q: "search" };
    const { result } = renderHook(() => useCatalogItems("core", params), {
      wrapper: createWrapper(qc),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(mockListCatalogItems).toHaveBeenCalledWith("core", params);
    expect(qc.getQueryData(["catalog-items", "core", params])).toEqual(itemList);
  });
});

describe("useComposedBundles", () => {
  it("is disabled without an accessible cluster", () => {
    const { result } = renderHook(() => useComposedBundles(null), { wrapper: createWrapper() });
    expect(result.current.fetchStatus).toBe("idle");
    expect(mockListComposedBundles).not.toHaveBeenCalled();
  });

  it("calls listComposedBundles and has queryKey ['composed-bundles', undefined]", async () => {
    const qc = createTestQueryClient();
    const bundleList: ComposedBundleList = {
      apiVersion: "varroa.dev/v1alpha1",
      kind: "ComposedBundleList",
      items: [createComposedBundle()],
    };
    mockListComposedBundles.mockResolvedValueOnce(bundleList);

    const { result } = renderHook(() => useComposedBundles("core"), {
      wrapper: createWrapper(qc),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(mockListComposedBundles).toHaveBeenCalledWith("core", undefined);
    expect(qc.getQueryData(["composed-bundles", "core", undefined])).toEqual(bundleList);
  });
});
