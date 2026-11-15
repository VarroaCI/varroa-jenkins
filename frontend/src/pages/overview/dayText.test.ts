import { describe, it, expect } from "vitest";
import { failedHoursLabel, jenkinsLine, nextScheduledLine, runningParts, shortDuration, totalsParts } from "./dayText";
import { fleetRun, hourly, NOW, totals } from "../../test/overviewFactories";

describe("dayText", () => {
  it("splits the totals into counters and a median without a zero second part", () => {
    expect(totalsParts(totals())).toEqual({ runs: 12, passed: 10, failed: 2, median: "1m" });
    expect(totalsParts(totals({ medianDurationMs: 0 }))?.median).toBe("0s");
    expect(totalsParts(totals({ runs24h: 0 }))).toBeUndefined();
  });

  it("drops only a zero second part from a duration", () => {
    expect(shortDuration(120_000)).toBe("2m");
    expect(shortDuration(65_000)).toBe("1m 5s");
    expect(shortDuration(0)).toBe("0s");
  });

  it("names the failed hours in the bar label, or says there was no failure", () => {
    const withFailures = failedHoursLabel(hourly([5, 11]));
    expect(withFailures.startsWith("Runs per hour, last 24 hours. Failures at ")).toBe(true);
    expect(withFailures.endsWith(".")).toBe(true);
    expect(withFailures.split("Failures at ")[1].split(", ")).toHaveLength(2);
    expect(failedHoursLabel(hourly())).toBe("Runs per hour, last 24 hours. No failures.");
  });

  it("names the first running run with its stage and elapsed time, and the count of the rest", () => {
    expect(runningParts([], NOW)).toEqual({ name: "", detail: "Nothing is running." });
    const running = [
      { run: fleetRun({ runId: "r1", status: "RUNNING", result: undefined, buildNumber: 341, tenant: "payments", jobPath: "build", startedAt: new Date(NOW - 120_000).toISOString() }), stage: "Test" },
      { run: fleetRun({ runId: "r2", status: "RUNNING", result: undefined, tenant: "smoke-a", jobPath: "cron" }) },
    ];
    expect(runningParts(running, NOW)).toEqual({ name: "payments / build #341", detail: " · Test stage · 2m and 1 more" });
  });

  it("names the queue and the next scheduled run, or that none is scheduled", () => {
    expect(nextScheduledLine(0, undefined)).toBe("0 queued · no scheduled run");
    const text = nextScheduledLine(3, {
      tenant: "smoke-a",
      tenantFolder: "smoke-a",
      cluster: "core",
      jobPath: "smoke-a/cron-probe",
      at: "2026-11-14T12:01:00Z",
    });
    expect(text).toMatch(/^3 queued · next scheduled: smoke-a \/ cron-probe at \d\d:\d\d$/);
  });

  it("counts the connected, asleep, and stopped controllers", () => {
    expect(jenkinsLine({ connected: 6, total: 8, asleep: 2, stopped: 0 })).toBe("6 of 8 connected · 2 asleep");
    expect(jenkinsLine({ connected: 6, total: 8, asleep: 0, stopped: 0 })).toBe("6 of 8 connected");
    expect(jenkinsLine({ connected: 5, total: 8, asleep: 0, stopped: 3 })).toBe("5 of 8 connected · 3 stopped");
  });
});
