import { describe, it, expect } from "vitest";
import { queueWaitMs, sortedParams, TRIGGER_LABEL, QUEUE_WAIT_SHOWN_MS } from "./runMeta";

describe("queueWaitMs", () => {
  it("answers the gap from queue to start", () => {
    expect(queueWaitMs({ queuedAt: "2026-10-11T10:00:00Z", startedAt: "2026-10-11T10:00:12Z" })).toBe(12000);
  });

  it("answers undefined when either time is missing", () => {
    expect(queueWaitMs({ startedAt: "2026-10-11T10:00:12Z" })).toBeUndefined();
    expect(queueWaitMs({ queuedAt: "2026-10-11T10:00:00Z" })).toBeUndefined();
  });

  it("answers undefined when the start precedes the queue", () => {
    expect(queueWaitMs({ queuedAt: "2026-10-11T10:00:12Z", startedAt: "2026-10-11T10:00:00Z" })).toBeUndefined();
  });
});

describe("sortedParams", () => {
  it("lists parameters by name", () => {
    expect(sortedParams({ b: "2", a: "1" })).toEqual([
      ["a", "1"],
      ["b", "2"],
    ]);
  });

  it("answers an empty list for absent params", () => {
    expect(sortedParams(undefined)).toEqual([]);
  });
});

describe("constants", () => {
  it("names every trigger source", () => {
    expect(Object.keys(TRIGGER_LABEL).sort()).toEqual(
      ["cron", "other", "replay", "restart", "scm", "upstream", "user", "webhook"],
    );
  });

  it("shows queue waits above five seconds", () => {
    expect(QUEUE_WAIT_SHOWN_MS).toBe(5000);
  });
});
