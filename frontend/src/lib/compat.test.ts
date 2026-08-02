import { describe, expect, it } from "vitest";
import {
  comparePluginVersions,
  groupByPlugin,
  isWarningVerdict,
  worstVerdict,
} from "./compat";
import type { CatalogItemSummary } from "../types";

const item = (pluginName: string, version: string): CatalogItemSummary => ({
  name: `uc-${pluginName}-${version}`,
  namespace: "varroa-system",
  sourceRef: "varroa-update-center",
  type: "plugin",
  valid: true,
  pluginName,
  version,
});

describe("comparePluginVersions", () => {
  it("compares numeric runs numerically, not lexically", () => {
    expect(comparePluginVersions("1.10.0", "1.3.0")).toBeGreaterThan(0);
    expect(comparePluginVersions("1.3.0", "1.10.0")).toBeLessThan(0);
  });

  it("treats a missing component as zero", () => {
    expect(comparePluginVersions("1.0", "1")).toBe(0);
    expect(comparePluginVersions("1.1", "1")).toBeGreaterThan(0);
  });

  it("orders an incrementals suffix rather than ignoring it", () => {
    // The whole reason this is not a core-version comparison: truncating at the
    // first '-' would make these equal.
    expect(comparePluginVersions("4.5.14-269.vfa_2321039a_83", "4.5.14")).not.toBe(0);
    expect(
      comparePluginVersions("1413.v2ff1a_5e720fa_", "1384.vdc05a_48f535f"),
    ).toBeGreaterThan(0);
  });

  it("ranks a numeric run above a qualifier", () => {
    expect(comparePluginVersions("1.1", "1.beta")).toBeGreaterThan(0);
  });
});

describe("worstVerdict", () => {
  it("returns null when there is nothing to judge", () => {
    expect(worstVerdict(undefined)).toBeNull();
    expect(worstVerdict([])).toBeNull();
  });

  it("applies the operator's precedence order", () => {
    expect(
      worstVerdict([
        { profile: "a", verdict: "lock-too-old" },
        { profile: "b", verdict: "dep-below-minimum" },
        { profile: "c", verdict: "compatible" },
      ]),
    ).toBe("dep-below-minimum");
    expect(
      worstVerdict([
        { profile: "a", verdict: "unknown" },
        { profile: "b", verdict: "core-too-old" },
      ]),
    ).toBe("core-too-old");
  });
});

describe("isWarningVerdict", () => {
  it("warns on the three concrete problems and nothing else", () => {
    expect(isWarningVerdict("core-too-old")).toBe(true);
    expect(isWarningVerdict("dep-below-minimum")).toBe(true);
    expect(isWarningVerdict("lock-too-old")).toBe(true);
    expect(isWarningVerdict("unknown")).toBe(false);
    expect(isWarningVerdict("compatible")).toBe(false);
  });
});

describe("groupByPlugin", () => {
  it("collapses versions of one plugin into a single group", () => {
    const groups = groupByPlugin([
      item("acme-widget", "1.2.0"),
      item("acme-widget", "1.10.0"),
      item("mailer", "2.0"),
    ]);
    expect(groups.map((g) => g.pluginName)).toEqual(["acme-widget", "mailer"]);
    expect(groups[0].versions).toHaveLength(2);
  });

  it("defaults a collapsed row to the highest version by the comparator", () => {
    const groups = groupByPlugin([
      item("acme-widget", "1.2.0"),
      item("acme-widget", "1.10.0"),
      item("acme-widget", "1.3.0"),
    ]);
    expect(groups[0].versions[0].version).toBe("1.10.0");
  });

  it("defaults to the pinned version when exactly one lock mentions the plugin", () => {
    const groups = groupByPlugin(
      [item("acme-widget", "1.2.0"), item("acme-widget", "1.10.0")],
      { "acme-widget": ["1.2.0"] },
    );
    expect(groups[0].versions[0].version).toBe("1.2.0");
  });

  it("falls back to the comparator when more than one lock mentions the plugin", () => {
    const groups = groupByPlugin(
      [item("acme-widget", "1.2.0"), item("acme-widget", "1.10.0")],
      { "acme-widget": ["1.2.0", "1.10.0"] },
    );
    expect(groups[0].versions[0].version).toBe("1.10.0");
  });

  it("both rules agree in the single-stored-version case", () => {
    const only = [item("acme-widget", "1.2.0")];
    expect(groupByPlugin(only)[0].versions[0].version).toBe("1.2.0");
    expect(
      groupByPlugin(only, { "acme-widget": ["1.2.0"] })[0].versions[0].version,
    ).toBe("1.2.0");
  });

  it("falls back to the item name when pluginName is absent", () => {
    const legacy = { ...item("x", "1.0"), pluginName: undefined };
    expect(groupByPlugin([legacy])[0].pluginName).toBe(legacy.name);
  });
});
