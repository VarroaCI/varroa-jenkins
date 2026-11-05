import { useCallback, useRef, useState } from "react";
import type { SSEEvent } from "./useEventStream";

/** FrameQueue is what useFrameQueue returns. */
export interface FrameQueue<T> {
  /** Pushes one frame and changes tick. Pass it to useEventStream as options.onEvent. Its identity never changes. */
  onEvent: (ev: SSEEvent<T>) => void;
  /** Returns every queued frame, oldest first, and empties the queue. Its identity never changes. */
  drain: () => SSEEvent<T>[];
  /** Empties the queue. It leaves tick unchanged. Its identity never changes. */
  clear: () => void;
  /** Changes once for each pushed frame. */
  tick: number;
}

/**
 * useFrameQueue keeps every frame of a stream until the caller reads it.
 * useEventStream calls onEvent once for each frame, also for each frame of a
 * burst that reaches React as one render. lastEvent keeps only the last frame
 * of that burst, so a caller that must read every frame uses this queue.
 *
 * A caller passes onEvent to useEventStream. It reads the frames in one effect
 * that depends on tick, and that effect calls drain. Call drain only inside an
 * effect: React can render a component more than once, and a drain in a render
 * loses frames. A caller calls clear when its scope changes, so a frame of the
 * old stream never reaches the new scope. clear starts no render.
 */
export function useFrameQueue<T>(): FrameQueue<T> {
  const queueRef = useRef<SSEEvent<T>[]>([]);
  const [tick, setTick] = useState(0);
  const onEvent = useCallback((ev: SSEEvent<T>) => {
    queueRef.current.push(ev);
    setTick((n) => n + 1);
  }, []);
  const drain = useCallback(() => {
    const frames = queueRef.current;
    queueRef.current = [];
    return frames;
  }, []);
  const clear = useCallback(() => {
    queueRef.current = [];
  }, []);
  return { onEvent, drain, clear, tick };
}
