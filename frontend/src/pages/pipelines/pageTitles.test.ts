import { describe, expect, it } from "vitest";
import {
  buildTitle,
  indexTitle,
  jobTitle,
  notStartedTitle,
  relativeJob,
  teamTitle,
  testCaseTitle,
} from "./pageTitles";

describe("pageTitles", () => {
  it("strips the tenant folder from the job path", () => {
    expect(relativeJob("checkout-api", "checkout-api/main")).toBe("main");
    expect(jobTitle("payments", "checkout-api", "checkout-api/release/eu")).toBe("release/eu · payments");
  });

  it("keeps the whole path when the relative path is empty", () => {
    expect(relativeJob("checkout-api", "checkout-api")).toBe("checkout-api");
    expect(relativeJob("", "main")).toBe("main");
  });

  it("puts the state first in a build title", () => {
    const run = { buildNumber: 42, tenantFolder: "checkout-api", jobPath: "checkout-api/main" };
    expect(buildTitle("payments", run, "Passed")).toBe("Passed · #42 main · payments");
  });

  it("shows #? when the build has no number", () => {
    const run = { tenantFolder: "", jobPath: "main" };
    expect(buildTitle("payments", run, "Running")).toBe("Running · #? main · payments");
  });

  it("names a refused trigger Not started", () => {
    expect(notStartedTitle("payments", "", "main")).toBe("Not started · main · payments");
  });

  it("builds the team and index titles", () => {
    expect(teamTitle("payments")).toBe("payments · Pipelines");
    expect(indexTitle()).toBe("Pipelines");
  });

  it("builds a test case title from the test, job and team", () => {
    expect(testCaseTitle("payments", "main", "adds totals")).toBe("adds totals · main · payments");
  });
});
