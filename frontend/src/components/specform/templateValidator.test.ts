import { describe, it, expect } from "vitest";
import { templateValidator } from "./templateValidator";

// A minimal 2020-12 pipeline-template parameters schema (design D3's subset).
const schema = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object" as const,
  additionalProperties: false,
  properties: {
    goVersion: {
      type: "string" as const,
      enum: ["1.22", "1.23", "1.24"],
    },
  },
  required: ["goVersion"],
};

describe("templateValidator", () => {
  it("accepts data that satisfies a 2020-12 schema", () => {
    const result = templateValidator.validateFormData({ goVersion: "1.24" }, schema);
    expect(result.errors).toHaveLength(0);
  });

  it("rejects a value outside the declared enum", () => {
    const result = templateValidator.validateFormData({ goVersion: "9.9" }, schema);
    expect(result.errors.length).toBeGreaterThan(0);
  });

  it("rejects a property additionalProperties: false does not declare", () => {
    const result = templateValidator.validateFormData(
      { goVersion: "1.24", extra: true },
      schema,
    );
    expect(result.errors.length).toBeGreaterThan(0);
  });

  it("rejects data missing a required property", () => {
    const result = templateValidator.validateFormData({}, schema);
    expect(result.errors.length).toBeGreaterThan(0);
  });
});
