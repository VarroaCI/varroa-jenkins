import { renderHook, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";
import type { ClusterEntry } from "../types";
import { useConfigurationCluster } from "./useConfigurationCluster";

const mockUseClusters = vi.fn();
vi.mock("./useClusters", async (importOriginal) => {
  const original = await importOriginal<typeof import("./useClusters")>();
  return { ...original, useClusters: () => mockUseClusters() };
});

const cluster = (name: string, core = false): ClusterEntry => ({
  name,
  core,
  healthy: true,
  state: "active",
  lastHeartbeat: "2025-01-01T00:00:00Z",
  operatorVersion: "1",
  k8sVersion: "1",
  controllerCount: 0,
  connectedCount: 0,
});

function wrapper(initialEntry: string) {
  return ({ children }: { children: ReactNode }) => (
    <MemoryRouter initialEntries={[initialEntry]}>{children}</MemoryRouter>
  );
}

describe("useConfigurationCluster", () => {
  beforeEach(() => mockUseClusters.mockReturnValue({ data: [cluster("edge"), cluster("hub", true)], isLoading: false }));

  it("keeps an eligible requested cluster", () => {
    const { result } = renderHook(useConfigurationCluster, { wrapper: wrapper("/catalog?cluster=edge") });
    expect(result.current.cluster).toBe("edge");
  });

  it("falls back to core and preserves other query parameters", async () => {
    const { result } = renderHook(() => ({ selection: useConfigurationCluster(), location: useLocation() }), {
      wrapper: wrapper("/catalog?cluster=missing&q=agents"),
    });
    await waitFor(() => expect(result.current.location.search).toBe("?cluster=hub&q=agents"));
    expect(result.current.selection.cluster).toBe("hub");
  });

  it("falls back to the first eligible cluster", () => {
    mockUseClusters.mockReturnValue({ data: [cluster("edge"), { ...cluster("hub", true), healthy: false }], isLoading: false });
    const { result } = renderHook(useConfigurationCluster, { wrapper: wrapper("/catalog?cluster=missing") });
    expect(result.current.cluster).toBe("edge");
  });

  it("waits for discovery and returns no selection when none is eligible", () => {
    mockUseClusters.mockReturnValue({ data: [], isLoading: false });
    const { result } = renderHook(useConfigurationCluster, { wrapper: wrapper("/catalog?cluster=edge") });
    expect(result.current).toEqual({ cluster: null, entry: null, ready: true });
  });
});
