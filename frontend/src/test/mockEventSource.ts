import { afterEach, beforeEach, vi } from "vitest";

/**
 * MockEventSource stands in for the browser EventSource in tests. It keeps
 * every instance so a test can drive the frames of the stream it opened.
 */
export class MockEventSource {
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

  /** emitNamedEvent delivers a named frame, with its SSE id when one is given. */
  emitNamedEvent(type: string, data: string, lastEventId = "") {
    this.listeners.get(type)?.(new MessageEvent(type, { data, lastEventId }));
  }

  emitError() {
    this.onerror?.(new Event("error"));
  }
}

/**
 * installMockEventSource replaces the global EventSource for every test in
 * the calling file, and restores it afterwards.
 */
export function installMockEventSource() {
  beforeEach(() => {
    MockEventSource.instances = [];
    vi.stubGlobal("EventSource", MockEventSource as unknown as typeof EventSource);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });
}
