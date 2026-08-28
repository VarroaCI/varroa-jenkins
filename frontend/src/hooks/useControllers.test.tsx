import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { useControllers, useController } from "./useControllers";
import { createControllerListItem, createControllerDetail } from "../test/factories";

// Mock bffFetch and bffFetchText so we control all network responses.
const mockBffFetch = vi.fn();
const mockBffFetchText = vi.fn();
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  bffFetchText: (...args: unknown[]) => mockBffFetchText(...args),
}));

function createWrapper(queryClient = createTestQueryClient()) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  mockBffFetch.mockReset();
});

describe("useControllers", () => {
  it("calls fetch and has queryKey ['controllers']", async () => {
    const qc = createTestQueryClient();
    const item = createControllerListItem();
    mockBffFetch.mockResolvedValueOnce({items: [item]});

    const { result } = renderHook(() => useControllers(), { wrapper: createWrapper(qc) });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(mockBffFetch).toHaveBeenCalledWith("/controllers");
    expect(result.current.data).toEqual([item]);
    expect(qc.getQueryData(["controllers"])).toEqual([item]);
  });

  it("returns loading state while pending", () => {
    // Return a promise that never settles so the query stays in loading state.
    mockBffFetch.mockReturnValueOnce(new Promise(() => {}));

    const { result } = renderHook(() => useControllers(), {
      wrapper: createWrapper(),
    });

    expect(result.current.isLoading).toBe(true);
  });

  it("returns error state when fetch fails", async () => {
    mockBffFetch.mockRejectedValueOnce(new Error("Network error"));

    const { result } = renderHook(() => useControllers(), {
      wrapper: createWrapper(),
    });

    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.error).toBeDefined();
  });
});

describe("useController", () => {
  it("fetches a single controller and has correct queryKey", async () => {
    const qc = createTestQueryClient();
    const detail = createControllerDetail({ name: "my-ctrl", namespace: "my-ns" });
    mockBffFetch.mockResolvedValueOnce(detail);

    const { result } = renderHook(() => useController("core", "my-ns", "my-ctrl"), {
      wrapper: createWrapper(qc),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    expect(mockBffFetch).toHaveBeenCalledWith("/clusters/core/controllers/my-ns/my-ctrl");
    expect(result.current.data).toEqual(detail);
    expect(qc.getQueryData(["controller", "core", "my-ns", "my-ctrl"])).toEqual(detail);
  });

  it("handles loading state", () => {
    mockBffFetch.mockReturnValueOnce(new Promise(() => {}));

    const { result } = renderHook(() => useController("core", "ns", "ctrl"), {
      wrapper: createWrapper(),
    });

    expect(result.current.isLoading).toBe(true);
  });

  it("handles error state", async () => {
    mockBffFetch.mockRejectedValueOnce(new Error("Not found"));

    const { result } = renderHook(() => useController("core", "ns", "ctrl"), {
      wrapper: createWrapper(),
    });

    await waitFor(() => expect(result.current.isError).toBe(true));
  });
});
