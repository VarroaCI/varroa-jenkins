import { describe, it, expect } from "vitest";
import { upgradeInfo, versionsDiffer } from "./versionCatalog";
import type { VersionCatalogEntry } from "../types";

// version-descending catalog (D's ordering guarantee).
const catalog: VersionCatalogEntry[] = [
  { version: "2.570", channel: "weekly", name: "2.570" },
  { version: "2.555", channel: "lts", recommended: true, name: "2.555" },
  { version: "2.541", channel: "lts", eol: "2026-04-15", name: "2.541" },
];

describe("upgradeInfo", () => {
  it("flags a recommended upgrade newer than the desired version", () => {
    const info = upgradeInfo("2.541", catalog);
    expect(info.managed).toBe(true);
    expect(info.recommendedUpgrade).toBe("2.555");
  });

  it("does not recommend when desired is the recommended entry itself", () => {
    const info = upgradeInfo("2.555", catalog);
    expect(info.managed).toBe(true);
    expect(info.recommendedUpgrade).toBeUndefined();
  });

  it("does not recommend a recommended entry older than desired", () => {
    // desired 2.570 (newest); recommended 2.555 is older → no upgrade.
    const info = upgradeInfo("2.570", catalog);
    expect(info.recommendedUpgrade).toBeUndefined();
  });

  it("returns unmanaged when desired is absent from the catalog", () => {
    expect(upgradeInfo("9.999", catalog)).toEqual({ managed: false });
  });

  it("marks a past eol date as passed", () => {
    const info = upgradeInfo("2.541", catalog);
    expect(info.eol).toBe("2026-04-15");
    expect(info.eolPassed).toBe(true);
  });

  it("marks a future eol date as not passed", () => {
    const future: VersionCatalogEntry[] = [
      { version: "2.600", channel: "lts", eol: "2099-01-01", name: "2.600" },
    ];
    const info = upgradeInfo("2.600", future);
    expect(info.eol).toBe("2099-01-01");
    expect(info.eolPassed).toBe(false);
  });
});

describe("versionsDiffer", () => {
  it("false when equal", () => {
    expect(versionsDiffer("2.552", "2.552")).toBe(false);
  });

  it("false when one is a dotted prefix of the other (line vs patch)", () => {
    expect(versionsDiffer("2.552.1", "2.552")).toBe(false);
    expect(versionsDiffer("2.552", "2.552.1")).toBe(false);
  });

  it("true for genuinely different versions", () => {
    expect(versionsDiffer("2.552", "2.540")).toBe(true);
  });

  it("false when either is undefined", () => {
    expect(versionsDiffer(undefined, "2.552")).toBe(false);
    expect(versionsDiffer("2.552", undefined)).toBe(false);
    expect(versionsDiffer(undefined, undefined)).toBe(false);
  });

  it("does not treat a non-dotted prefix as a version prefix", () => {
    // "2.55" is a string-prefix of "2.552" but not a dotted-segment prefix.
    expect(versionsDiffer("2.55", "2.552")).toBe(true);
  });
});
