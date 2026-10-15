import { describe, it, expect } from "vitest";
import { stripTenantFolder, testCaseHref } from "./jobPath";

describe("testCaseHref", () => {
  it("encodes the job path and the case key as one segment each", () => {
    expect(testCaseHref("payments", "payments/checkout-api/main", "com.example.FooTest/bar")).toBe(
      "/pipelines/payments/payments%2Fcheckout-api%2Fmain/tests/com.example.FooTest%2Fbar",
    );
  });

  it("encodes a percent sign the case key already holds once more", () => {
    expect(testCaseHref("payments", "main", "a%2Fb/c")).toBe("/pipelines/payments/main/tests/a%252Fb%2Fc");
  });
});

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
