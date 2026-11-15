import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { useOverviewLive, GROUPS_WINDOW_MS, OVERVIEW_WINDOW_MS } from "./useOverviewLive";
import { createTestQueryClient } from "../../test/render-utils";

const stream = vi.hoisted(() => ({
  readyState: "closed",
  calls: [] as { url: string | null; scope: string | null; events: string[]; options: { onEvent?: (ev: unknown) => void } }[],
}));

vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: (url: string | null, scope: string | null, events: string[], options: { onEvent?: (ev: unknown) => void }) => {
    stream.calls.push({ url, scope, events, options });
    return { lastEvent: null, readyState: stream.readyState, error: null };
  },
}));

vi.mock("../../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../../api/client")>()),
  fleetRunStreamUrl: () => "/api/v1/pipelines/runs/stream",
}));

function setup(enabled: boolean) {
  const client: QueryClient = createTestQueryClient();
  const spy = vi.spyOn(client, "invalidateQueries").mockResolvedValue(undefined);
  const wrapper = ({ children }: { children: React.ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  );
  const hook = renderHook(({ on }: { on: boolean }) => useOverviewLive(on), {
    wrapper,
    initialProps: { on: enabled },
  });
  return { hook, spy };
}

function lastOnEvent(): (ev: unknown) => void {
  const call = stream.calls[stream.calls.length - 1];
  return call.options.onEvent!;
}

describe("useOverviewLive", () => {
  beforeEach(() => {
    stream.readyState = "closed";
    stream.calls = [];
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it("opens one pipelines-runs stream when enabled", () => {
    setup(true);
    const call = stream.calls[stream.calls.length - 1];
    expect(call.url).toBe("/api/v1/pipelines/runs/stream");
    expect(call.scope).toBe("pipelines-runs");
    expect(call.events).toEqual(["run", "input", "lock"]);
  });

  it("opens no stream when disabled", () => {
    setup(false);
    const call = stream.calls[stream.calls.length - 1];
    expect(call.url).toBeNull();
    expect(call.scope).toBeNull();
  });

  it("a burst invalidates the overview once per window", () => {
    const { spy } = setup(true);
    act(() => {
      lastOnEvent()({ type: "input", data: {} });
      lastOnEvent()({ type: "lock", data: {} });
      lastOnEvent()({ type: "input", data: {} });
    });
    expect(spy).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(OVERVIEW_WINDOW_MS);
    });
    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalledWith({ queryKey: ["pipelines-overview"] });

    act(() => {
      lastOnEvent()({ type: "input", data: {} });
      vi.advanceTimersByTime(OVERVIEW_WINDOW_MS);
    });
    expect(spy).toHaveBeenCalledTimes(2);
  });

  it("a run frame invalidates the run groups", () => {
    const { spy } = setup(true);
    act(() => {
      lastOnEvent()({ type: "run", data: {} });
    });
    act(() => {
      vi.advanceTimersByTime(GROUPS_WINDOW_MS);
    });
    expect(spy).toHaveBeenCalledWith({ queryKey: ["pipelines-run-groups"] });
  });

  it("a reconnect invalidates both reads", () => {
    stream.readyState = "open";
    const { hook, spy } = setup(true);
    // The first open reads nothing: the page already read its own data.
    expect(spy).not.toHaveBeenCalled();
    stream.readyState = "reconnecting";
    hook.rerender({ on: true });
    stream.readyState = "open";
    hook.rerender({ on: true });
    expect(spy).toHaveBeenCalledWith({ queryKey: ["pipelines-overview"] });
    expect(spy).toHaveBeenCalledWith({ queryKey: ["pipelines-run-groups"] });
  });
});
