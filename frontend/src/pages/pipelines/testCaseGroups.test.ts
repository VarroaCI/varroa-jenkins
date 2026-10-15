import { describe, it, expect } from "vitest";
import type { RunTestCase } from "../../types";
import { groupTestCases } from "./testCaseGroups";

function tc(suite: string, className: string, name: string, status = "FAILED"): RunTestCase {
  return { suite, className, name, status, caseKey: `${className}/${name}` };
}

describe("groupTestCases", () => {
  it("groups consecutive cases by suite in the response order", () => {
    const { groups } = groupTestCases([tc("unit", "A", "a1"), tc("unit", "B", "b1"), tc("api", "C", "c1")]);
    expect(groups.map((g) => g.suite)).toEqual(["unit", "api"]);
    expect(groups[0].rows.map((r) => r.name)).toEqual(["a1", "b1"]);
    expect(groups[1].rows.map((r) => r.name)).toEqual(["c1"]);
  });

  it("starts a new group when a suite returns after another suite", () => {
    const { groups } = groupTestCases([
      tc("unit", "A", "failed", "FAILED"),
      tc("api", "B", "other", "FAILED"),
      tc("unit", "A", "skipped", "SKIPPED"),
    ]);
    expect(groups.map((g) => [g.suite, g.rows.map((r) => r.name)])).toEqual([
      ["unit", ["failed"]],
      ["api", ["other"]],
      ["unit", ["skipped"]],
    ]);
  });

  it("hides the class column when every group shares one class", () => {
    const { groups, showClassColumn } = groupTestCases([
      tc("github.com/x/alpha", "github.com/x/alpha", "TestA"),
      tc("github.com/x/beta", "github.com/x/beta", "TestB"),
    ]);
    expect(showClassColumn).toBe(false);
    expect(groups.map((g) => g.sharedClassName)).toEqual(["github.com/x/alpha", "github.com/x/beta"]);
  });

  it("shows the class column when one group holds two classes", () => {
    const { groups, showClassColumn } = groupTestCases([tc("unit", "A", "a"), tc("unit", "B", "b")]);
    expect(showClassColumn).toBe(true);
    expect(groups[0].sharedClassName).toBeUndefined();
  });

  it("labels a group with its class when the class and suite differ only by separators", () => {
    const { groups } = groupTestCases([tc("github.com_x_y", "github.com/x/y", "TestA")]);
    expect(groups[0].label).toBe("github.com/x/y");
    expect(groups[0].secondary).toBeUndefined();
  });

  it("labels a group with its suite and names a different shared class as secondary text", () => {
    const { groups } = groupTestCases([tc("unit", "com.example.Shared", "a"), tc("unit", "com.example.Shared", "b")]);
    expect(groups[0].label).toBe("unit");
    expect(groups[0].secondary).toBe("com.example.Shared");
    expect(groups[0].sharedClassName).toBe("com.example.Shared");
  });
});
