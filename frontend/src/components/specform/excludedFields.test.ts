import { describe, it, expect } from "vitest";
import { EXCLUDED_FROM_TIER1 } from "./excludedFields";

describe("EXCLUDED_FROM_TIER1", () => {
  it("contains the expected field names", () => {
    expect(EXCLUDED_FROM_TIER1).toEqual([
      "version",
      "hibernation",
      "powerState",
      "probes",
      "composedBundleRef",
      "reconciliationPolicy",
      "podOverrides",
      "resourceOverlay",
      "ingressSpec",
      "miteSpec",
      "endpoint",
    ]);
  });

  it("does not contain Tier 1 typed fields", () => {
    const tier1Fields = ["rbacSpec", "pluginSpec", "backupSpec", "resources", "persistence", "className"];
    for (const f of tier1Fields) {
      expect(EXCLUDED_FROM_TIER1).not.toContain(f);
    }
  });

  it("has exactly 11 entries", () => {
    expect(EXCLUDED_FROM_TIER1).toHaveLength(11);
  });
});
