import { describe, it, expect } from "vitest";
import { getControllerSpecSchema, getPodOverridesSchema } from "./openapiSchema";
import fixture from "./__fixtures__/openapi.json";

describe("getControllerSpecSchema", () => {
  it("returns a schema with properties from the fixture", () => {
    const schema = getControllerSpecSchema(fixture as unknown as Record<string, unknown>);
    expect(schema).toBeDefined();
    expect((schema as Record<string, unknown>).type).toBe("object");
    const props = ((schema as Record<string, unknown>).properties as Record<string, unknown>) ?? {};
    expect(props).toHaveProperty("rbacSpec");
    expect(props).toHaveProperty("ingressSpec");
    expect(props).toHaveProperty("version");
    expect(props).toHaveProperty("podOverrides");
  });

  it("pre-resolves $ref references so the schema has no $ref", () => {
    const schema = getControllerSpecSchema(fixture as unknown as Record<string, unknown>);
    expect(schema).toBeDefined();

    function hasRef(obj: unknown): boolean {
      if (!obj || typeof obj !== "object") return false;
      if (Array.isArray(obj)) return obj.some(hasRef);
      const d = obj as Record<string, unknown>;
      if (d["$ref"]) return true;
      return Object.values(d).some(hasRef);
    }

    expect(hasRef(schema)).toBe(false);
  });

  it("returns undefined for undefined root", () => {
    expect(getControllerSpecSchema(undefined)).toBeUndefined();
  });
});

describe("getPodOverridesSchema", () => {
  it("returns a schema with at least one real property", () => {
    const schema = getPodOverridesSchema(fixture as unknown as Record<string, unknown>);
    expect(schema).toBeDefined();
    expect((schema as Record<string, unknown>).type).toBe("object");
    const props = ((schema as Record<string, unknown>).properties as Record<string, unknown>) ?? {};
    expect(props).toHaveProperty("env");
    expect(props).toHaveProperty("jvmOpts");
    // Should not have probe fields
    expect(props).not.toHaveProperty("livenessProbe");
    expect(props).not.toHaveProperty("readinessProbe");
    expect(props).not.toHaveProperty("startupProbe");
  });

  it("returns undefined for undefined root", () => {
    expect(getPodOverridesSchema(undefined)).toBeUndefined();
  });
});
