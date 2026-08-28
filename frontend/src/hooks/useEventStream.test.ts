import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
import { useEventStream } from "./useEventStream";
import { bffFetch } from "./useApi";

vi.mock("./useApi", () => ({ bffFetch: vi.fn() }));
const mockBffFetch = vi.mocked(bffFetch);

// ---- Mock EventSource aligned with the hook's property-handler usage ----
class MockEventSource {
  static instances: MockEventSource[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: MessageEvent) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  listeners = new Map<string, (e: MessageEvent) => void>();
  url: string;
  wasClosed = false;

  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
  }
  close() {
    this.wasClosed = true;
  }
  addEventListener(type: string, listener: (e: MessageEvent) => void) {
    this.listeners.set(type, listener);
  }
  emitOpen() {
    this.onopen?.();
  }
  emitMessage(data: string) {
    this.onmessage?.(new MessageEvent("message", { data }));
  }
  emitNamedEvent(type: string, data: string) {
    this.listeners.get(type)?.(new MessageEvent(type, { data }));
  }
  emitError() {
    this.onerror?.(new Event("error"));
  }
}

beforeEach(() => {
  MockEventSource.instances = [];
  vi.stubGlobal("EventSource", MockEventSource as unknown as typeof EventSource);
  mockBffFetch.mockReset();
  mockBffFetch.mockResolvedValue({ ticket: "vst_abc", expiresInSeconds: 30 });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("useEventStream", () => {
  it("does not connect when baseUrl or scope is null", () => {
    renderHook(() => useEventStream("/x", null));
    renderHook(() => useEventStream(null, "brood"));
    expect(mockBffFetch).not.toHaveBeenCalled();
    expect(MockEventSource.instances).toHaveLength(0);
  });

  it("mints a scoped ticket then connects with ?ticket=", async () => {
    renderHook(() => useEventStream("/api/v1/stream/brood", "brood"));
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));

    expect(mockBffFetch).toHaveBeenCalledWith(
      "/stream/ticket",
      expect.objectContaining({ method: "POST" }),
    );
    const body = JSON.parse((mockBffFetch.mock.calls[0][1] as RequestInit).body as string);
    expect(body.scope).toBe("brood");
    expect(MockEventSource.instances[0].url).toContain("ticket=vst_abc");
    // A session token must never appear in the stream URL.
    expect(MockEventSource.instances[0].url).not.toContain("token=");
  });

  it("appends &ticket= when the base URL already has a query", async () => {
    renderHook(() => useEventStream("/u?follow=true", "brood"));
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    expect(MockEventSource.instances[0].url).toContain("follow=true&ticket=");
  });

  it("parses messages into lastEvent", async () => {
    const { result } = renderHook(() => useEventStream<{ x: number }>("/u", "brood"));
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];
    act(() => {
      es.emitOpen();
      es.emitMessage(JSON.stringify({ type: "activity", data: { x: 1 } }));
    });
    await waitFor(() => expect(result.current.lastEvent?.type).toBe("activity"));
    expect(result.current.lastEvent?.data).toEqual({ x: 1 });
  });

  it("parses named events into {type, data} via addEventListener", async () => {
    const { result } = renderHook(() =>
      useEventStream<{ x: number }>("/u", "activity", ["activity"]),
    );
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];
    act(() => {
      es.emitOpen();
      es.emitNamedEvent("activity", JSON.stringify({ x: 42 }));
    });
    await waitFor(() => expect(result.current.lastEvent?.type).toBe("activity"));
    expect(result.current.lastEvent?.data).toEqual({ x: 42 });
  });

  it("still handles unnamed message frames when eventNames are also passed", async () => {
    const { result } = renderHook(() =>
      useEventStream<{ line: string }>("/u", "logs", ["status", "closed"]),
    );
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];
    act(() => {
      es.emitOpen();
      // A named frame routes through addEventListener...
      es.emitNamedEvent("status", JSON.stringify({ line: "a" }));
      // ...and an unnamed frame still routes through onmessage (the logs
      // stream's bare `data:` frames are parsed straight into lastEvent).
      es.emitMessage(JSON.stringify({ type: "log", data: { line: "b" } }));
    });
    await waitFor(() => expect(result.current.lastEvent?.data).toEqual({ line: "b" }));
  });

  it("does not re-create the stream when an equal eventNames array is passed on rerender", async () => {
    const { rerender } = renderHook(
      ({ names }: { names: string[] }) => useEventStream<{ x: number }>("/u", "activity", names),
      { initialProps: { names: ["activity"] } },
    );
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));

    // useActivityFeed passes a fresh ["activity"] literal on every render; the
    // hook must normalize it to a stable key rather than tear down and re-mint
    // the stream (which would reconnect on every ingested event).
    rerender({ names: ["activity"] });
    await new Promise((r) => setTimeout(r, 50));
    expect(MockEventSource.instances).toHaveLength(1);
  });

  it("closes the stream on unmount", async () => {
    const { unmount } = renderHook(() => useEventStream("/u", "brood"));
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];
    unmount();
    expect(es.wasClosed).toBe(true);
  });

  it("surfaces a failed ticket mint (e.g. a 403) as `error` instead of connecting silently", async () => {
    mockBffFetch.mockReset();
    mockBffFetch.mockRejectedValue(new Error("403 Forbidden"));

    const { result } = renderHook(() => useEventStream("/u", "brood"));

    await waitFor(() => expect(result.current.error?.message).toBe("403 Forbidden"));
    expect(MockEventSource.instances).toHaveLength(0);
    expect(result.current.readyState).not.toBe("open");
  });

  it("sets `error` on an EventSource-level failure and clears it once the connection opens", async () => {
    const { result } = renderHook(() => useEventStream("/u", "brood"));
    await waitFor(() => expect(MockEventSource.instances).toHaveLength(1));
    const es = MockEventSource.instances[0];

    act(() => {
      es.emitError();
    });
    await waitFor(() => expect(result.current.error).not.toBeNull());
    expect(result.current.readyState).toBe("closed");

    act(() => {
      es.emitOpen();
    });
    await waitFor(() => expect(result.current.error).toBeNull());
    expect(result.current.readyState).toBe("open");
  });
});
