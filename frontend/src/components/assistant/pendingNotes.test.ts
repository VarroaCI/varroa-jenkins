import { describe, it, expect, afterEach, vi } from "vitest";
import { pendingNotes } from "./pendingNotes";

afterEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe("pendingNotes", () => {
  it("keys the store by subject and survives a reload", () => {
    const store = pendingNotes("alice");
    store.add({ sessionId: "s1", runId: "r1", at: 1000 });
    expect(window.localStorage.getItem("varroa-assistant-pending-notes:alice")).toContain("s1");
    expect(pendingNotes("alice").list()).toEqual([{ sessionId: "s1", runId: "r1", at: 1000 }]);
    expect(pendingNotes("bob").list()).toEqual([]);
  });

  it("replaces a note for the same session and removes by session", () => {
    const store = pendingNotes("alice");
    store.add({ sessionId: "s1", runId: "r1", at: 1 });
    store.add({ sessionId: "s1", runId: "r2", at: 2 });
    expect(store.list()).toEqual([{ sessionId: "s1", runId: "r2", at: 2 }]);
    store.remove("s1");
    expect(store.list()).toEqual([]);
  });

  it("drops a note older than a day when pruned", () => {
    const store = pendingNotes("alice");
    const now = 10_000_000_000;
    store.add({ sessionId: "old", runId: "r", at: now - 25 * 60 * 60 * 1000 });
    store.add({ sessionId: "new", runId: "r", at: now - 1000 });
    store.prune(now);
    expect(store.list().map((n) => n.sessionId)).toEqual(["new"]);
  });

  it("answers an empty list for a corrupt or non-array value", () => {
    window.localStorage.setItem("varroa-assistant-pending-notes:alice", "{not json");
    expect(pendingNotes("alice").list()).toEqual([]);
    window.localStorage.setItem("varroa-assistant-pending-notes:alice", "{}");
    expect(pendingNotes("alice").list()).toEqual([]);
  });

  it("never throws when the store is blocked", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    const store = pendingNotes("alice");
    expect(store.list()).toEqual([]);
    expect(() => store.add({ sessionId: "s", runId: "r" })).not.toThrow();
    expect(() => store.remove("s")).not.toThrow();
    expect(() => store.prune()).not.toThrow();
  });
});
