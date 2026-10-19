import { describe, it, expect } from "vitest";
import { contextDiffers, contextLabel, routeContext } from "./routeContext";

describe("routeContext", () => {
  it.each([
    ["/pipelines/payments/payments%2Fapi/tests/case1", { route: "test-case", team: "payments", job: "payments/api" }],
    ["/pipelines/payments/payments%2Fapi/0192f0e4", { route: "build", team: "payments", job: "payments/api", run: "0192f0e4" }],
    ["/pipelines/payments/payments%2Fapi", { route: "job", team: "payments", job: "payments/api" }],
    ["/pipelines/payments", { route: "team", team: "payments" }],
    ["/controllers/core/ci/main", { route: "controller", controller: "core/ci/main" }],
    ["/cells", { route: "cells" }],
    ["/locks", { route: "locks" }],
    ["/activity", { route: "activity" }],
    ["/", { route: "dashboard" }],
    ["/settings/users", { route: "settings" }],
  ])("maps %s", (path, want) => {
    expect(routeContext(path)).toEqual(want);
  });

  it("names a cell when the page hint says so", () => {
    expect(routeContext("/controllers/core/ci/main", { cell: true })).toEqual({
      route: "controller",
      controller: "core/ci/main",
      cell: "core/ci/main",
    });
  });

  it("returns dashboard for an empty path", () => {
    expect(routeContext("")).toEqual({ route: "dashboard" });
  });

  it("compares the objects a context names", () => {
    expect(contextDiffers({ team: "a" }, { team: "a" })).toBe(false);
    expect(contextDiffers({ team: "a" }, { team: "b" })).toBe(true);
    expect(contextDiffers({ run: "1" }, {})).toBe(true);
  });

  it("labels each route", () => {
    expect(contextLabel({ route: "build", team: "p", job: "j", run: "0192f0e4abcdef" })).toBe("Build 0192f0e4 of p/j");
    expect(contextLabel({ route: "job", team: "p", job: "j" })).toBe("Job p/j");
    expect(contextLabel({ route: "team", team: "p" })).toBe("Team p");
    expect(contextLabel({ route: "controller", controller: "c/n/x" })).toBe("Controller c/n/x");
    expect(contextLabel({ route: "controller", controller: "c/n/x", cell: "c/n/x" })).toBe("Cell c/n/x");
    expect(contextLabel({ route: "cells" })).toBe("Cells");
    expect(contextLabel({ route: "locks" })).toBe("Locks");
    expect(contextLabel({ route: "activity" })).toBe("Activity");
    expect(contextLabel({ route: "dashboard" })).toBe("Dashboard");
    expect(contextLabel({ route: "settings" })).toBe("settings");
    expect(contextLabel({})).toBe("");
  });
});
