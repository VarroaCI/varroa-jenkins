import { describe, it, expect } from "vitest";
import { savedProviderGone } from "./providerChoice";

const SMOKE = { name: "smoke", kind: "openai-compatible", models: ["varroa-smoke"], trust: "internal", default: true, available: true, baseURL: "http://x" };

describe("savedProviderGone", () => {
  it("is false while the provider list is not loaded", () => {
    expect(savedProviderGone("gone", [], false)).toBe(false);
  });

  it("is false for Default", () => {
    expect(savedProviderGone("", [SMOKE], true)).toBe(false);
  });

  it("is false for a listed provider", () => {
    expect(savedProviderGone("smoke", [SMOKE], true)).toBe(false);
  });

  it("is true for a saved provider that is not listed", () => {
    expect(savedProviderGone("gone", [SMOKE], true)).toBe(true);
  });
});
