import { describe, it, expect } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useEffect } from "react";
import { useFrameQueue } from "./useFrameQueue";
import type { SSEEvent } from "./useEventStream";

const frame = (n: number): SSEEvent<number> => ({ type: "run", data: n });

// useDrainedBatches drains in an effect that depends on tick, as a real caller
// does, and records each non-empty batch.
function useDrainedBatches(batches: SSEEvent<number>[][]) {
  const queue = useFrameQueue<number>();
  const { drain, tick } = queue;
  useEffect(() => {
    const frames = drain();
    if (frames.length > 0) batches.push(frames);
  }, [tick, drain]);
  return queue;
}

describe("useFrameQueue", () => {
  it("drains a burst of three frames in one effect run", () => {
    const batches: SSEEvent<number>[][] = [];
    const { result } = renderHook(() => useDrainedBatches(batches));
    act(() => {
      result.current.onEvent(frame(1));
      result.current.onEvent(frame(2));
      result.current.onEvent(frame(3));
    });
    expect(batches).toEqual([[frame(1), frame(2), frame(3)]]);
    expect(result.current.tick).toBe(3);
  });

  it("hands each frame to one drain only", () => {
    const batches: SSEEvent<number>[][] = [];
    const { result, rerender } = renderHook(() => useDrainedBatches(batches));
    act(() => result.current.onEvent(frame(1)));
    rerender();
    act(() => result.current.onEvent(frame(2)));
    expect(batches).toEqual([[frame(1)], [frame(2)]]);
    expect(result.current.drain()).toEqual([]);
  });

  it("clear drops the queued frames and starts no render", () => {
    let renders = 0;
    const { result } = renderHook(() => {
      renders += 1;
      return useFrameQueue<number>();
    });
    act(() => result.current.onEvent(frame(1)));
    const before = renders;
    act(() => result.current.clear());
    expect(renders).toBe(before);
    expect(result.current.tick).toBe(1);
    expect(result.current.drain()).toEqual([]);
  });

  it("keeps onEvent, drain, and clear identical across renders", () => {
    const { result, rerender } = renderHook(() => useFrameQueue<number>());
    const { onEvent, drain, clear } = result.current;
    act(() => result.current.onEvent(frame(1)));
    rerender();
    expect(result.current.onEvent).toBe(onEvent);
    expect(result.current.drain).toBe(drain);
    expect(result.current.clear).toBe(clear);
  });
});
