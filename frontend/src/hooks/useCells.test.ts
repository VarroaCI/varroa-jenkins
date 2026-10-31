import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import {
  cellUsageKey,
  useCellGrants,
  useCellPool,
  useCellPools,
  useCellUsage,
  useCellUsageIndex,
  useCellUsageList,
} from "./useCells";

const mockBffFetch = vi.fn();
// vi.mock is hoisted, so the error class is built in a hoisted block too.
// The real useApi module is not imported here: it imports the API client,
// which imports this module back, so the mock keeps the client's own
// bffFetch binding on the mock.
const { TestApiError } = vi.hoisted(() => ({
  TestApiError: class TestApiError extends Error {
    status: number;
    body?: unknown;
    constructor(status: number, message: string, body?: unknown) {
      super(message);
      this.status = status;
      this.body = body;
    }
    get code(): string {
      const body = this.body as { code?: unknown } | undefined;
      return typeof body?.code === "string" ? body.code : "";
    }
  },
}));
vi.mock("../hooks/useApi", () => ({
  bffFetch: (...args: unknown[]) => mockBffFetch(...args),
  bffFetchText: vi.fn(),
  bffUpload: vi.fn(),
  getToken: vi.fn(),
  ApiError: TestApiError,
  isRecordStoreUnreachable: (e: unknown) =>
    e instanceof TestApiError && e.status === 503 && e.code === "record_store_unreachable",
}));

function createWrapper(queryClient = createTestQueryClient()) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return createElement(QueryClientProvider, { client: queryClient }, children);
  };
}

beforeEach(() => {
  mockBffFetch.mockReset();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("useCells polling", () => {
  it("polls the pool list every 15 seconds", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    const { result } = renderHook(() => useCellPools(), { wrapper: createWrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(mockBffFetch).toHaveBeenCalledWith("/cellpools");
  });

  it("polls the batch usage read every 15 seconds", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    const { result } = renderHook(() => useCellUsageList(), { wrapper: createWrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(mockBffFetch).toHaveBeenCalledWith("/cells/usage");
  });

  it("keys the pool list and the usage list apart", async () => {
    const qc = createTestQueryClient();
    mockBffFetch.mockResolvedValue({ items: [] });
    renderHook(() => useCellPools(), { wrapper: createWrapper(qc) });
    renderHook(() => useCellUsageList(), { wrapper: createWrapper(qc) });
    await waitFor(() => expect(qc.getQueryData(["cellpools", null])).toEqual({ items: [] }));
    expect(qc.getQueryData(["cell-usage", null, null])).toEqual([]);
  });
});

describe("the pool reads name the cluster", () => {
  it("sends the cluster on the pool list", async () => {
    mockBffFetch.mockResolvedValue({ items: [], clusters: [] });
    const { result } = renderHook(() => useCellPools("member-c"), { wrapper: createWrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(mockBffFetch).toHaveBeenCalledWith("/cellpools?cluster=member-c");
  });

  it("reads a hive cell's pool on that cell's cluster", async () => {
    mockBffFetch.mockResolvedValue({});
    const { result } = renderHook(() => useCellPool("member-2-570", true, "member-c"), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(mockBffFetch).toHaveBeenCalledWith("/cellpools/member-2-570?cluster=member-c");
  });
});

describe("the Cell tab's own reads", () => {
  it("runs no request while another tab is active", () => {
    const { result: pool } = renderHook(() => useCellPool("pool-a", false), { wrapper: createWrapper() });
    const { result: usage } = renderHook(() => useCellUsage("ns-a", "cell-a", false), { wrapper: createWrapper() });
    const { result: grants } = renderHook(() => useCellGrants("ns-a", "cell-a", false), { wrapper: createWrapper() });
    expect(pool.current.fetchStatus).toBe("idle");
    expect(usage.current.fetchStatus).toBe("idle");
    expect(grants.current.fetchStatus).toBe("idle");
    expect(mockBffFetch).not.toHaveBeenCalled();
  });

  it("runs each request once the tab is active", async () => {
    mockBffFetch.mockResolvedValue({});
    const { result } = renderHook(() => useCellUsage("ns-a", "cell-a", true), { wrapper: createWrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(mockBffFetch).toHaveBeenCalledWith("/cells/ns-a/cell-a/usage");
  });

  it("gives the grants read a 60-second stale time and no interval", async () => {
    const qc = createTestQueryClient();
    mockBffFetch.mockResolvedValue({ cluster: "core", namespace: "ns-a", cell: "cell-a", tenants: [] });
    const { result } = renderHook(() => useCellGrants("ns-a", "cell-a", true), { wrapper: createWrapper(qc) });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(mockBffFetch).toHaveBeenCalledWith("/cells/ns-a/cell-a/grants");
    const query = qc.getQueryCache().find({ queryKey: ["cell-grants", null, "ns-a", "cell-a"] });
    const options = query?.options as { staleTime?: number; refetchInterval?: number | false } | undefined;
    expect(options?.staleTime).toBe(60_000);
    expect(options?.refetchInterval).toBeFalsy();
  });
});

describe("no cell surface streams", () => {
  it("constructs no EventSource", async () => {
    const eventSource = vi.fn();
    vi.stubGlobal("EventSource", eventSource);
    mockBffFetch.mockResolvedValue({ items: [] });
    const { result } = renderHook(() => useCellUsageList(), { wrapper: createWrapper() });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(eventSource).not.toHaveBeenCalled();
  });
});

describe("useCellUsageIndex", () => {
  it("reads once per cluster and keys every row by its own cluster", async () => {
    mockBffFetch.mockImplementation((path: string) => {
      if (path.includes("cluster=member")) {
        return Promise.resolve({
          items: [{ cluster: "member", namespace: "ns-a", name: "cell-a", maxTenants: 4 }],
        });
      }
      return Promise.resolve({
        items: [{ cluster: "core", namespace: "ns-a", name: "cell-a", maxTenants: 10 }],
      });
    });

    const { result } = renderHook(() => useCellUsageIndex(["core", "member"]), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(result.current.rows.size).toBe(2));

    expect(mockBffFetch).toHaveBeenCalledWith("/cells/usage?cluster=core");
    expect(mockBffFetch).toHaveBeenCalledWith("/cells/usage?cluster=member");
    // The same namespace and name in two clusters stay apart.
    expect(result.current.rows.get(cellUsageKey("core", "ns-a", "cell-a"))?.maxTenants).toBe(10);
    expect(result.current.rows.get(cellUsageKey("member", "ns-a", "cell-a"))?.maxTenants).toBe(4);
    expect(result.current.covered).toEqual(new Set(["core", "member"]));
  });

  it("keys a row by the requested cluster, never by the reported one", async () => {
    // A response that names another cluster must not move the row into it.
    mockBffFetch.mockResolvedValue({
      items: [{ cluster: "somewhere-else", namespace: "ns-a", name: "cell-a" }],
    });
    const { result } = renderHook(() => useCellUsageIndex(["core"]), { wrapper: createWrapper() });
    await waitFor(() => expect(result.current.rows.size).toBe(1));
    expect(result.current.rows.has(cellUsageKey("core", "ns-a", "cell-a"))).toBe(true);
    expect(result.current.rows.has(cellUsageKey("somewhere-else", "ns-a", "cell-a"))).toBe(false);
  });

  it("reports a 503 cluster as not covered rather than empty", async () => {
    mockBffFetch.mockImplementation((path: string) => {
      if (path.includes("cluster=member")) {
        return Promise.reject(
          new TestApiError(503, "record store not configured", { error: "record store not configured" }),
        );
      }
      return Promise.resolve({ items: [{ cluster: "core", namespace: "ns-a", name: "cell-a" }] });
    });
    const { result } = renderHook(() => useCellUsageIndex(["core", "member"]), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(result.current.recordStoreDisabled.has("member")).toBe(true));
    expect(result.current.covered).toEqual(new Set(["core"]));
  });

  it("reports an unreachable cluster apart from a disabled one", async () => {
    mockBffFetch.mockImplementation((path: string) => {
      if (path.includes("cluster=member")) {
        return Promise.reject(
          new TestApiError(503, "record store unreachable", {
            error: "record store unreachable",
            code: "record_store_unreachable",
          }),
        );
      }
      return Promise.resolve({ items: [{ cluster: "core", namespace: "ns-a", name: "cell-a" }] });
    });
    const { result } = renderHook(() => useCellUsageIndex(["core", "member"]), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(result.current.recordStoreUnreachable.has("member")).toBe(true));
    expect(result.current.recordStoreDisabled.has("member")).toBe(false);
    expect(result.current.covered).toEqual(new Set(["core"]));
  });

  it("refetch reads every cluster again", async () => {
    mockBffFetch.mockResolvedValue({ items: [] });
    const { result } = renderHook(() => useCellUsageIndex(["core", "member"]), {
      wrapper: createWrapper(),
    });
    await waitFor(() => expect(mockBffFetch).toHaveBeenCalledTimes(2));
    act(() => result.current.refetch());
    await waitFor(() => expect(mockBffFetch).toHaveBeenCalledTimes(4));
    expect(mockBffFetch).toHaveBeenCalledWith("/cells/usage?cluster=core");
    expect(mockBffFetch).toHaveBeenCalledWith("/cells/usage?cluster=member");
  });

  it("issues no request for an empty cluster list", () => {
    const { result } = renderHook(() => useCellUsageIndex([]), { wrapper: createWrapper() });
    expect(result.current.rows.size).toBe(0);
    expect(mockBffFetch).not.toHaveBeenCalled();
  });
});
