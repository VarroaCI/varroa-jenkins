import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { parse as parseYAML } from "yaml";
import { getDefaultFormState, type RJSFSchema } from "@rjsf/utils";
import { templateValidator } from "./templateValidator";

// The shared golden corpus design D15 describes: the Go validator, the
// plugin's Java validator, and this browser validator each walk the same
// fixture files from their own test suite, so a change to the corpus
// changes the parity claim for all three. This file lives at
// frontend/src/components/specform/, so four levels up is the repo root.
const corpusDir = join(
  import.meta.dirname,
  "../../../../internal/bundle/pipelinetemplate/testdata/schema-corpus",
);
const valuesDir = join(corpusDir, "values");
const subsetDir = join(corpusDir, "subset");

interface ValuesCase {
  id: string;
  schema: string;
  values: string;
  verdict: "accept" | "reject";
  expectDefaults?: Record<string, unknown>;
}

interface ValuesManifest {
  cases: ValuesCase[];
}

// A subset case's manifest entry carries id/file/verdict/contains and, for a
// reject case whose finding text Go and the plugin agree on byte for byte,
// `message` (schema-corpus/subset/manifest.yaml). Ajv renders its own
// error text, so this runner reads `message` only to accept the shape and
// never compares it. `reason`/`kind` are read here in case
// a sibling case class starts declaring one to say explicitly whether Ajv
// can be expected to reject a case on its own (a "structural" JSON-Schema
// violation) or only this project's subset policy does ("subset-only") —
// see the classification note below.
interface SubsetCase {
  id: string;
  file: string;
  verdict: "accept" | "reject";
  contains?: string;
  message?: string;
  reason?: string;
  kind?: string;
}

interface SubsetManifest {
  cases: SubsetCase[];
}

const KNOWN_SUBSET_KEYS = ["id", "file", "verdict", "contains", "message", "reason", "kind"];

function loadYAML<T>(path: string): T {
  return parseYAML(readFileSync(path, "utf8")) as T;
}

/** Attempts to compile `schema` with the Ajv instance behind
 * `templateValidator`, returning the thrown error, if any. Always removes
 * the schema afterward so one case's compile attempt cannot collide with
 * another's (Ajv caches a compiled schema by object reference and by
 * `$id`).
 */
function tryCompile(schema: RJSFSchema): unknown {
  let caught: unknown;
  try {
    templateValidator.ajv.compile(schema);
  } catch (err) {
    caught = err;
  } finally {
    try {
      templateValidator.ajv.removeSchema(schema);
    } catch {
      // Compile threw before the schema was cached; nothing to remove.
    }
  }
  return caught;
}

describe("templateValidator against the shared schema-corpus/values corpus", () => {
  const manifest = loadYAML<ValuesManifest>(join(valuesDir, "manifest.yaml"));

  it("loads at least one corpus case", () => {
    expect(manifest.cases.length).toBeGreaterThan(0);
  });

  for (const c of manifest.cases) {
    it(`${c.id}: ${c.verdict}`, () => {
      const schema = loadYAML<RJSFSchema>(join(valuesDir, c.schema));
      const values = loadYAML<Record<string, unknown>>(join(valuesDir, c.values));
      const result = templateValidator.validateFormData(values, schema);
      if (c.verdict === "accept") {
        expect(result.errors).toHaveLength(0);
      } else {
        expect(result.errors.length).toBeGreaterThan(0);
      }
      if (c.expectDefaults) {
        const defaulted = getDefaultFormState(
          templateValidator,
          schema,
          values,
          schema,
        ) as Record<string, unknown>;
        for (const [key, want] of Object.entries(c.expectDefaults)) {
          expect(defaulted[key]).toEqual(want);
        }
      }
    });
  }
});

// The values corpus above proves this validator agrees with Go on whether a
// document's *data* satisfies a schema. This corpus proves the companion
// question: whether the *schema* document itself is one this project's
// subset policy allows (design D3). Ajv is a general-purpose 2020-12
// validator: it enforces the JSON Schema spec, but it has no notion of the
// subset restriction Go's `CheckSchemaSubset` and the Java validator add on
// top of spec compliance. So an accepted subset schema must compile in Ajv,
// but a rejected one only fails to compile in Ajv when the rejection is a
// genuine spec violation ("structural"); a rejection that exists solely
// because of the subset policy ("subset-only", e.g. a spec-legal `$ref` or
// `format` keyword this project forbids) compiles fine in Ajv and cannot be
// proved false by this validator. Those cases are documented skips, counted
// below, rather than a false pass from an assertion that never ran.
describe("templateValidator against the shared schema-corpus/subset corpus", () => {
  const manifest = loadYAML<SubsetManifest>(join(subsetDir, "manifest.yaml"));

  it("loads at least one corpus case", () => {
    expect(manifest.cases.length).toBeGreaterThan(0);
  });

  let structuralRejectCount = 0;
  let subsetOnlyRejectCount = 0;

  for (const c of manifest.cases) {
    const unknownKeys = Object.keys(c).filter((k) => !KNOWN_SUBSET_KEYS.includes(k));
    if (unknownKeys.length > 0) {
      // A case shape this file has not seen (for example one that also
      // ships a uiSchema fixture) cannot be proved by compiling `file`
      // against Ajv alone. Fail loudly, by name, instead of silently
      // mis-testing or skipping it.
      it(`${c.id}: unrecognized case shape`, () => {
        throw new Error(
          `subset case "${c.id}" carries unrecognized field(s) [${unknownKeys.join(", ")}]; ` +
            "extend templateValidatorCorpus.test.ts to cover this case class",
        );
      });
      continue;
    }

    if (c.verdict !== "accept" && c.verdict !== "reject") {
      it(`${c.id}: unrecognized verdict`, () => {
        throw new Error(`subset case "${c.id}" carries unrecognized verdict ${JSON.stringify(c.verdict)}`);
      });
      continue;
    }

    const schema = loadYAML<RJSFSchema>(join(subsetDir, c.file));
    const compileErr = tryCompile(schema);

    if (c.verdict === "accept") {
      it(`${c.id}: accept`, () => {
        expect(compileErr).toBeUndefined();
      });
      continue;
    }

    const declared = c.reason ?? c.kind;
    if (declared !== undefined && declared !== "structural" && declared !== "subset-only") {
      it(`${c.id}: unrecognized reason/kind`, () => {
        throw new Error(`subset case "${c.id}" declares unrecognized reason/kind ${JSON.stringify(declared)}`);
      });
      continue;
    }

    if (declared === "structural" && compileErr === undefined) {
      // The manifest asserts this is a spec violation Ajv should also
      // catch, but Ajv compiled the schema anyway: a real behavior gap
      // between the shared corpus expectation and this browser validator.
      it(`${c.id}: declared structural but Ajv accepted the schema`, () => {
        throw new Error(
          `subset case "${c.id}" is declared structural (reason/kind: structural) ` +
            "but Ajv compiled its schema without error",
        );
      });
      continue;
    }

    const isSubsetOnly = declared === "subset-only" || (declared === undefined && compileErr === undefined);
    if (isSubsetOnly) {
      subsetOnlyRejectCount += 1;
      it.skip(
        `${c.id}: reject (subset-only — this project's policy, not a JSON Schema violation Ajv can catch)`,
        () => {},
      );
    } else {
      structuralRejectCount += 1;
      it(`${c.id}: reject (Ajv also fails to compile)`, () => {
        expect(compileErr).toBeDefined();
      });
    }
  }

  it("classifies every reject case as structural or subset-only", () => {
    const rejectCount = manifest.cases.filter((c) => c.verdict === "reject").length;
    expect(structuralRejectCount + subsetOnlyRejectCount).toBe(rejectCount);
  });
});
