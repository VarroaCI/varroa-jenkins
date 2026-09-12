import { describe, it, expect } from "vitest";
import { stripTenantFolder } from "./jobPath";

describe("stripTenantFolder", () => {
  it("returns jobPath unchanged for a root-hosted tenant", () => {
    expect(stripTenantFolder("", "checkout-api/main")).toBe("checkout-api/main");
  });

  it("strips the tenant folder prefix from a nested job path", () => {
    expect(stripTenantFolder("payments", "payments/checkout-api/main")).toBe("checkout-api/main");
  });

  it("returns the empty string when jobPath equals the tenant folder", () => {
    expect(stripTenantFolder("payments", "payments")).toBe("");
  });

  it("returns jobPath unchanged when it does not carry the tenant folder prefix", () => {
    expect(stripTenantFolder("payments", "other/checkout-api/main")).toBe("other/checkout-api/main");
  });
});
