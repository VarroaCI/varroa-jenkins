import { describe, it, expect } from "vitest";
import {
  age,
  laneFor,
  eventKey,
  mergeEvents,
  resultMeta,
  groupKey,
  groupEvents,
  passesScope,
  passesSelection,
  passesLane,
  passesSource,
  GROUP_MAX,
  MAX_BUFFER,
} from "./activityTimeline.util";
import type { ActivityEvent } from "../types";

function makeEvent(overrides: Partial<ActivityEvent>): ActivityEvent {
  return {
    timestamp: new Date().toISOString(),
    type: "build.completed",
    source: "jenkins",
    message: "Build completed",
    ...overrides,
  };
}

describe("laneFor", () => {
  it("returns control for operator source", () => {
    expect(laneFor(makeEvent({ source: "operator" }))).toBe("control");
  });

  it("returns control for mite source", () => {
    expect(laneFor(makeEvent({ source: "mite" }))).toBe("control");
  });

  it("returns control for user source", () => {
    expect(laneFor(makeEvent({ source: "user" }))).toBe("control");
  });

  it("returns builds for jenkins source with buildNumber", () => {
    expect(laneFor(makeEvent({ source: "jenkins", buildNumber: 42 }))).toBe("builds");
  });

  it("returns control for jenkins source without buildNumber", () => {
    expect(laneFor(makeEvent({ source: "jenkins", buildNumber: undefined }))).toBe("control");
  });

  it("returns control for unknown source (fail-safe)", () => {
    expect(laneFor(makeEvent({ source: "unknown-future-source" }))).toBe("control");
  });
});

describe("eventKey", () => {
  it("produces a deterministic composite key", () => {
    const e = makeEvent({
      timestamp: "2024-01-01T00:00:00Z",
      source: "jenkins",
      type: "build.completed",
      namespace: "ns",
      controller: "ctrl",
      buildNumber: 5,
      message: "done",
    });
    const key = eventKey(e);
    expect(key).toContain("2024-01-01T00:00:00Z");
    expect(key).toContain("jenkins");
    expect(key).toContain("build.completed");
    expect(key).toContain("ns");
    expect(key).toContain("ctrl");
    expect(key).toContain("5");
    expect(key).toContain("done");
  });
});

describe("mergeEvents", () => {
  it("deduplicates when same event arrives via both paths", () => {
    const e = makeEvent({ timestamp: "2024-01-01T00:00:00Z", buildNumber: 1 });
    const buffer: ActivityEvent[] = [e];
    const incoming: ActivityEvent[] = [e]; // same event
    const result = mergeEvents(buffer, incoming);
    expect(result).toHaveLength(1);
  });

  it("merges late backfill events after live SSE arrivals", () => {
    const live1 = makeEvent({ timestamp: "2024-01-01T00:00:03Z", buildNumber: 3 });
    const live2 = makeEvent({ timestamp: "2024-01-01T00:00:02Z", buildNumber: 2 });
    const backfill = makeEvent({ timestamp: "2024-01-01T00:00:01Z", buildNumber: 1 });
    // Live SSE arrives first
    const buffer: ActivityEvent[] = [live1, live2];
    // Backfill resolves later
    const result = mergeEvents(buffer, [backfill]);
    expect(result).toHaveLength(3);
    // Newest first
    expect(result[0].buildNumber).toBe(3);
    expect(result[1].buildNumber).toBe(2);
    expect(result[2].buildNumber).toBe(1);
  });

  it("caps buffer at MAX_BUFFER dropping oldest", () => {
    const events: ActivityEvent[] = [];
    for (let i = 0; i < MAX_BUFFER + 10; i++) {
      // Use proper ISO timestamps — offset each by i seconds
      const d = new Date("2024-01-01T00:00:00Z");
      d.setUTCSeconds(d.getUTCSeconds() + i);
      events.push(
        makeEvent({
          timestamp: d.toISOString(),
          buildNumber: i,
        }),
      );
    }
    // Put first 5 in the buffer, the rest as incoming
    const buffer = events.slice(0, 5);
    const incoming = events.slice(5);
    const result = mergeEvents(buffer, incoming, MAX_BUFFER);
    expect(result.length).toBeLessThanOrEqual(MAX_BUFFER);
    // Should have dropped oldest (lowest buildNumber = 0..9 since 2010 total)
    const buildNums = result.map((e) => e.buildNumber!);
    expect(buildNums).toContain(MAX_BUFFER + 9); // newest (2009)
    // One of the oldest should have been dropped
    expect(buildNums.length).toBe(MAX_BUFFER);
  });
});

describe("resultMeta", () => {
  it("maps SUCCESS to ✔/softOk", () => {
    const { icon, style } = resultMeta(makeEvent({ result: "SUCCESS" }));
    expect(icon).toBe("✔");
    expect(style).toBe("softOk");
  });

  it("maps FAILURE to ✘/softBad", () => {
    const { icon, style } = resultMeta(makeEvent({ result: "FAILURE" }));
    expect(icon).toBe("✘");
    expect(style).toBe("softBad");
  });

  it("maps UNSTABLE to ▲/softWarn", () => {
    const { icon, style } = resultMeta(makeEvent({ result: "UNSTABLE" }));
    expect(icon).toBe("▲");
    expect(style).toBe("softWarn");
  });

  it("maps ABORTED to ⊘/softMuted", () => {
    const { icon, style } = resultMeta(makeEvent({ result: "ABORTED" }));
    expect(icon).toBe("⊘");
    expect(style).toBe("softMuted");
  });

  it("maps NOT_BUILT to ◌/softMuted", () => {
    const { icon, style } = resultMeta(makeEvent({ result: "NOT_BUILT" }));
    expect(icon).toBe("◌");
    expect(style).toBe("softMuted");
  });

  it("maps absent result to ◷/softInfo (running)", () => {
    const { icon, style } = resultMeta(makeEvent({ result: undefined }));
    expect(icon).toBe("◷");
    expect(style).toBe("softInfo");
  });

  it("is case-insensitive via uppercase", () => {
    const { icon } = resultMeta(makeEvent({ result: "success" }));
    expect(icon).toBe("✔");
  });
});

describe("groupKey", () => {
  it("uses space separator to avoid collision", () => {
    const a = groupKey(makeEvent({ namespace: "a", controller: "bc" }));
    const b = groupKey(makeEvent({ namespace: "ab", controller: "c" }));
    // cluster="" ns="a",ctrl="bc" → " a bc"
    // cluster="" ns="ab",ctrl="c" → " ab c"
    expect(a).toBe(" a bc");
    expect(b).toBe(" ab c");
    expect(a).not.toBe(b);
  });
});

describe("groupEvents", () => {
  function ts(sec: number): string {
    const d = new Date("2024-01-01T00:00:00.000Z");
    d.setUTCSeconds(d.getUTCSeconds() + sec);
    return d.toISOString();
  }

  it("collapses adjacent build events for one controller", () => {
    const events: ActivityEvent[] = [
      makeEvent({ timestamp: ts(10), source: "jenkins", buildNumber: 1, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(9), source: "jenkins", buildNumber: 2, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(8), source: "jenkins", buildNumber: 3, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(7), source: "jenkins", buildNumber: 4, namespace: "ns", controller: "ctrlA" }),
    ];
    const rows = groupEvents(events);
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("group");
    if (rows[0].kind === "group") {
      expect(rows[0].members).toHaveLength(4);
    }
  });

  it("breaks group when gap exceeds GROUP_GAP_MS", () => {
    // Two members per group so we get group rows, not singleton events
    const events: ActivityEvent[] = [
      makeEvent({ timestamp: ts(200), source: "jenkins", buildNumber: 1, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(199), source: "jenkins", buildNumber: 2, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(10), source: "jenkins", buildNumber: 3, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(9), source: "jenkins", buildNumber: 4, namespace: "ns", controller: "ctrlA" }),
    ];
    // Gap between ts(199) and ts(10) = 189s = 189000ms > 120000ms GROUP_GAP_MS
    const rows = groupEvents(events);
    expect(rows).toHaveLength(2);
    expect(rows[0].kind).toBe("group");
    expect(rows[0].kind === "group" ? rows[0].members.length : 0).toBe(2);
    expect(rows[1].kind).toBe("group");
    expect(rows[1].kind === "group" ? rows[1].members.length : 0).toBe(2);
  });

  it("respects GROUP_MAX cap", () => {
    const events: ActivityEvent[] = [];
    for (let i = 0; i < GROUP_MAX + 5; i++) {
      events.push(
        makeEvent({
          timestamp: ts(1000 + i),
          source: "jenkins",
          buildNumber: i,
          namespace: "ns",
          controller: "ctrlA",
        }),
      );
    }
    const rows = groupEvents(events);
    // First group should have GROUP_MAX members, second group the remainder
    expect(rows.length).toBeGreaterThanOrEqual(2);
    if (rows[0].kind === "group") {
      expect(rows[0].members.length).toBe(GROUP_MAX);
    }
  });

  it("flushes group on control-plane interleave", () => {
    const events: ActivityEvent[] = [
      makeEvent({ timestamp: ts(10), source: "jenkins", buildNumber: 1, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(9), source: "jenkins", buildNumber: 2, namespace: "ns", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(8), source: "operator", type: "connected", message: "interleave" }),
      makeEvent({ timestamp: ts(7), source: "jenkins", buildNumber: 3, namespace: "ns", controller: "ctrlA" }),
    ];
    const rows = groupEvents(events);
    expect(rows).toHaveLength(3);
    // First group (2 builds)
    expect(rows[0].kind).toBe("group");
    // Control-plane event
    expect(rows[1].kind).toBe("event");
    // Second group (1 build → singleton event)
    expect(rows[2].kind).toBe("event");
  });

  it("does not merge same controller name in different namespaces", () => {
    const events: ActivityEvent[] = [
      makeEvent({ timestamp: ts(10), source: "jenkins", buildNumber: 1, namespace: "ns1", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(9), source: "jenkins", buildNumber: 2, namespace: "ns1", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(8), source: "jenkins", buildNumber: 3, namespace: "ns2", controller: "ctrlA" }),
      makeEvent({ timestamp: ts(7), source: "jenkins", buildNumber: 4, namespace: "ns2", controller: "ctrlA" }),
    ];
    const rows = groupEvents(events);
    expect(rows).toHaveLength(2);
    // Each should be its own group (different groupKey)
    expect(rows[0].kind).toBe("group");
    expect(rows[1].kind).toBe("group");
    // And they should have different groupKeys
    if (rows[0].kind === "group" && rows[1].kind === "group") {
      expect(rows[0].groupKey).not.toBe(rows[1].groupKey);
    }
  });

  it("renders singleton group as plain event row", () => {
    const events: ActivityEvent[] = [
      makeEvent({ timestamp: ts(10), source: "jenkins", buildNumber: 1, namespace: "ns", controller: "ctrlA" }),
    ];
    const rows = groupEvents(events);
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("event");
  });

  it("builds correct breakdown counts", () => {
    const events: ActivityEvent[] = [
      makeEvent({ timestamp: ts(10), source: "jenkins", buildNumber: 1, namespace: "ns", controller: "ctrlA", result: "SUCCESS" }),
      makeEvent({ timestamp: ts(9), source: "jenkins", buildNumber: 2, namespace: "ns", controller: "ctrlA", result: "SUCCESS" }),
      makeEvent({ timestamp: ts(8), source: "jenkins", buildNumber: 3, namespace: "ns", controller: "ctrlA", result: "FAILURE" }),
      makeEvent({ timestamp: ts(7), source: "jenkins", buildNumber: 4, namespace: "ns", controller: "ctrlA", result: undefined }), // running
    ];
    const rows = groupEvents(events);
    expect(rows).toHaveLength(1);
    if (rows[0].kind === "group") {
      expect(rows[0].breakdown).toEqual({
        SUCCESS: 2,
        FAILURE: 1,
        running: 1,
      });
    }
  });
});

describe("passesScope", () => {
  it("returns true when no scope set", () => {
    expect(passesScope(makeEvent({ source: "mite" }), undefined)).toBe(true);
  });

  it("returns true when event matches scope", () => {
    const e = makeEvent({ namespace: "ns", controller: "ctrl" });
    expect(passesScope(e, { namespace: "ns", name: "ctrl" })).toBe(true);
  });

  it("returns false when event namespace differs", () => {
    const e = makeEvent({ namespace: "other", controller: "ctrl" });
    expect(passesScope(e, { namespace: "ns", name: "ctrl" })).toBe(false);
  });

  it("returns false when event controller differs", () => {
    const e = makeEvent({ namespace: "ns", controller: "other" });
    expect(passesScope(e, { namespace: "ns", name: "ctrl" })).toBe(false);
  });
});

describe("passesSelection", () => {
  it("returns true when selection is empty", () => {
    expect(passesSelection(makeEvent({ source: "mite" }), new Set())).toBe(true);
  });

  it("returns true when event controller is in selection", () => {
    const e = makeEvent({ namespace: "ns", controller: "ctrlA" });
    expect(passesSelection(e, new Set(["/ns/ctrlA"]))).toBe(true);
  });

  it("returns false when event controller not in selection", () => {
    const e = makeEvent({ namespace: "ns", controller: "ctrlB" });
    expect(passesSelection(e, new Set(["/ns/ctrlA"]))).toBe(false);
  });

  it("returns true for global event when __global__ selected", () => {
    const e = makeEvent({ controller: undefined, namespace: undefined });
    expect(passesSelection(e, new Set(["__global__"]))).toBe(true);
  });

  it("returns false for global event when __global__ not selected", () => {
    const e = makeEvent({ controller: undefined, namespace: undefined });
    expect(passesSelection(e, new Set(["/ns/ctrlA"]))).toBe(false);
  });
});

describe("passesLane", () => {
  it("returns true for All", () => {
    expect(passesLane(makeEvent({ source: "jenkins", buildNumber: 1 }), "All")).toBe(true);
  });

  it("filters by lane", () => {
    const build = makeEvent({ source: "jenkins", buildNumber: 1 });
    expect(passesLane(build, "builds")).toBe(true);
    expect(passesLane(build, "control")).toBe(false);
  });
});

describe("passesSource", () => {
  it("returns true for All", () => {
    expect(passesSource(makeEvent({ source: "mite" }), "All")).toBe(true);
  });

  it("filters by source", () => {
    expect(passesSource(makeEvent({ source: "mite" }), "mite")).toBe(true);
    expect(passesSource(makeEvent({ source: "operator" }), "mite")).toBe(false);
  });
});

describe("age", () => {
  const now = Date.parse("2025-01-01T00:05:00Z");

  it("renders minute granularity by default", () => {
    expect(age("2025-01-01T00:04:30Z", { now })).toBe("just now");
    expect(age("2025-01-01T00:02:00Z", { now })).toBe("3m ago");
    expect(age("2025-01-01T00:00:00Z", { now: Date.parse("2025-01-01T03:00:00Z") })).toBe("3h ago");
    expect(age("2025-01-01T00:00:00Z", { now: Date.parse("2025-01-03T00:00:00Z") })).toBe("2d ago");
  });

  it("renders heartbeat tiers: seconds < 120s, minutes < 120m, then uncapped hours", () => {
    const hb = { variant: "heartbeat" as const };
    expect(age("2025-01-01T00:04:30Z", { ...hb, now })).toBe("30s ago");
    expect(age("2025-01-01T00:02:00Z", { ...hb, now })).toBe("3m ago");
    // 60–119m stays minute-granular (staleness window near the health TTL).
    expect(age("2025-01-01T00:00:00Z", { ...hb, now: Date.parse("2025-01-01T01:30:00Z") })).toBe("90m ago");
    expect(age("2025-01-01T00:00:00Z", { ...hb, now: Date.parse("2025-01-01T03:00:00Z") })).toBe("3h ago");
    // No day tier for heartbeats: hours stay uncapped.
    expect(age("2025-01-01T00:00:00Z", { ...hb, now: Date.parse("2025-01-02T06:00:00Z") })).toBe("30h ago");
  });

  it("returns em-dash for unparseable heartbeat input", () => {
    expect(age("garbage", { variant: "heartbeat", now })).toBe("—");
    expect(age("", { variant: "heartbeat", now })).toBe("—");
  });

  it("returns em-dash for future heartbeat timestamps", () => {
    expect(age("2025-01-01T00:10:00Z", { variant: "heartbeat", now })).toBe("—");
  });
});
