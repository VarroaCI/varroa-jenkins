import { describe, it, expect } from "vitest";
import { controllerCounts, overviewSubtitle } from "./overviewText";
import type { ControllerListItem } from "../../hooks/useControllers";

describe("overviewSubtitle", () => {
  it("names both counts, tenants first", () => {
    expect(overviewSubtitle({ tenants: 4, controllers: { controllers: 12, namespaces: 3 } })).toBe(
      "4 tenants · 12 controllers across 3 namespaces",
    );
  });

  it("uses singular nouns", () => {
    expect(overviewSubtitle({ tenants: 1, controllers: { controllers: 1, namespaces: 1 } })).toBe(
      "1 tenant · 1 controller across 1 namespace",
    );
  });

  it("leaves out an undefined part", () => {
    expect(overviewSubtitle({ tenants: 0 })).toBe("0 tenants");
    expect(overviewSubtitle({ controllers: { controllers: 2, namespaces: 1 } })).toBe(
      "2 controllers across 1 namespace",
    );
  });

  it("answers an empty string with no part", () => {
    expect(overviewSubtitle({})).toBe("");
  });
});

describe("controllerCounts", () => {
  it("counts (cluster, namespace) identities", () => {
    const row = (cluster: string, namespace: string) =>
      ({ cluster, namespace, name: "c", phase: "Running", miteConnected: true, endpoint: "" }) as ControllerListItem;
    expect(controllerCounts([row("core", "varroa"), row("b", "varroa"), row("core", "varroa")])).toEqual({
      controllers: 3,
      namespaces: 2,
    });
  });
});
