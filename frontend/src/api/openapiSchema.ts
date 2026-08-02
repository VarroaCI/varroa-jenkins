import { useQuery } from "@tanstack/react-query";
import { bffFetch } from "../hooks/useApi";

/**
 * Fetches the bundled OpenAPI spec from the BFF.
 */
export function fetchOpenAPISchema(): Promise<Record<string, unknown>> {
  return bffFetch<Record<string, unknown>>("/openapi.json");
}

/**
 * React Query hook to fetch the OpenAPI spec once (cached forever — spec only
 * changes on BFF redeploy).
 */
export function useOpenAPISchema() {
  return useQuery({
    queryKey: ["openapi", "schema"],
    queryFn: fetchOpenAPISchema,
    staleTime: Infinity,
  });
}

/**
 * Recursively resolve all `$ref` references in a schema tree against the
 * bundled spec's `components.schemas` map. This handles both the bundled
 * format (`#/components/schemas/<flat-name>`) and the raw format (`#/<name>`).
 */
function resolveSchemaRefs(
  obj: unknown,
  schemas: Record<string, unknown>,
  visited?: Set<string>,
): unknown {
  if (visited === undefined) visited = new Set();
  if (!obj || typeof obj !== "object") return obj;

  if (Array.isArray(obj)) {
    return obj.map((item) => resolveSchemaRefs(item, schemas, visited));
  }

  const dict = obj as Record<string, unknown>;
  const ref = dict["$ref"] as string | undefined;

  if (ref && typeof ref === "string" && ref.startsWith("#")) {
    // Extract schema name from the ref path
    const parts = ref.replace(/^#\//, "").split("/");
    let name = parts[parts.length - 1]; // last segment is the schema name

    // Try shorter forms: after stripping 'components/schemas/' prefix
    if (parts.length >= 3 && parts[0] === "components" && parts[1] === "schemas") {
      // For bundled refs like #/components/schemas/components_schemas_RBACSpec
      // The name IS already the last part
    } else if (parts.length === 1) {
      name = parts[0];
    }

    if (name && schemas[name] && !visited.has(name)) {
      visited.add(name);
      const resolved = JSON.parse(JSON.stringify(schemas[name]));
      return resolveSchemaRefs(resolved, schemas, visited);
    }
    // If we can't resolve, return the ref as-is (defense-in-depth)
    return obj;
  }

  const result: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(dict)) {
    result[key] = resolveSchemaRefs(value, schemas, visited);
  }
  return result;
}

/**
 * Resolves a named schema from the bundled OpenAPI spec's `components.schemas`
 * map, deep-cloning and resolving all `$ref` references so RJSF / AJV can
 * render or validate without external resolution.
 */
function getNamedSchema(
  root: Record<string, unknown> | undefined,
  name: string,
): Record<string, unknown> | undefined {
  if (!root) return undefined;
  const components = root.components as Record<string, unknown> | undefined;
  if (!components) return undefined;
  const schemas = components.schemas as Record<string, unknown> | undefined;
  if (!schemas) return undefined;
  const spec = schemas[name] as Record<string, unknown> | undefined;
  if (!spec) return undefined;
  // Deep-clone and resolve all $ref references
  const clone = JSON.parse(JSON.stringify(spec));
  const resolved = resolveSchemaRefs(clone, schemas) as Record<string, unknown>;
  return resolved;
}

/**
 * Resolves the ControllerSpec schema, pre-resolving all `$ref` references so
 * RJSF can render without external resolution.
 */
export function getControllerSpecSchema(
  root: Record<string, unknown> | undefined,
): Record<string, unknown> | undefined {
  return getNamedSchema(root, "ControllerSpec");
}

/**
 * Resolves PodOverrides schema (for Tier 2 AJV validation) from the bundled
 * OpenAPI spec.
 */
export function getPodOverridesSchema(
  root: Record<string, unknown> | undefined,
): Record<string, unknown> | undefined {
  return getNamedSchema(root, "PodOverrides");
}

/**
 * Resolves IngressSpec schema (for the ingressSpec YAML tier) from the
 * bundled OpenAPI spec.
 */
export function getIngressSpecSchema(
  root: Record<string, unknown> | undefined,
): Record<string, unknown> | undefined {
  return getNamedSchema(root, "IngressSpec");
}

/**
 * Resolves MiteSpec schema (for the miteSpec YAML tier) from the bundled
 * OpenAPI spec.
 */
export function getMiteSpecSchema(
  root: Record<string, unknown> | undefined,
): Record<string, unknown> | undefined {
  return getNamedSchema(root, "MiteSpec");
}
