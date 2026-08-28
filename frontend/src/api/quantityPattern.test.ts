import { describe, it, expect } from "vitest";
import Ajv from "ajv";
import { getControllerSpecSchema } from "./openapiSchema";
import fixture from "./__fixtures__/openapi.json";

// Schema-level AJV test for the Kubernetes quantity pattern on
// ControllerSpec.resources.limits/.requests. `resources` currently renders no
// inputs in the curated form (the RJSF map editor is a later change), so this
// is deliberately a schema test, not a typing/interaction test. It depends on
// the fixture mirroring the canonical bundled document (see
// openapiFixture.test.ts), which is why 7.2 lands before this.
describe("ControllerSpec resource quantity pattern", () => {
  const schema = getControllerSpecSchema(fixture as unknown as Record<string, unknown>);
  if (!schema) throw new Error("fixture must resolve a ControllerSpec schema");

  const validate = new Ajv({ strict: false, allErrors: true }).compile(schema);

  it("rejects an invalid quantity (12qq) in limits", () => {
    const valid = validate({ resources: { limits: { cpu: "12qq" } } });
    expect(valid).toBe(false);
    expect(validate.errors?.some((e) => e.keyword === "pattern")).toBe(true);
  });

  it("rejects an invalid quantity in requests", () => {
    expect(validate({ resources: { requests: { memory: "1zz" } } })).toBe(false);
  });

  it("accepts a millicore quantity (100m)", () => {
    expect(validate({ resources: { limits: { cpu: "100m" } } })).toBe(true);
  });

  it("accepts a binary-suffix quantity (1Gi)", () => {
    expect(validate({ resources: { requests: { memory: "1Gi" } } })).toBe(true);
  });

  it("accepts a plain integer quantity (4)", () => {
    expect(validate({ resources: { limits: { cpu: "4" } } })).toBe(true);
  });
});
