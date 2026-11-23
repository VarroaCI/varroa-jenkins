import { describe, expect, it } from "vitest";
import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { parse as parseYAML } from "yaml";

// The Detect changes job of pr.yaml starts the Frontend test job only for the
// paths that its frontend and frontendextra rules name. A frontend test that
// reads a file outside frontend/ must name that file in those rules, or a
// change to the file alone skips the frontend tests. This test finds each such
// read and fails when a workflow copy does not name it.
//
// The scan finds a string literal that starts with "../" and resolves outside
// frontend/. Name an outside file with one such literal, relative to the
// directory of the test file.

// This file lives at frontend/src/, so ".." is the frontend root.
const frontendRoot = resolve(import.meta.dirname, "..");
const repoRoot = resolve(frontendRoot, "..");

// The first path is the workflow of this checkout. The second path is a copy
// that becomes .github/workflows/pr.yaml of another tree. That tree has no
// hack/public-overlay/ directory, so the check of that copy skips its paths.
// A checkout without the copy skips the check of the copy.
const liveWorkflow = join(import.meta.dirname, "../../.github/workflows/pr.yaml");
const overlayWorkflow = join(import.meta.dirname, "../../hack/public-overlay/.github/workflows/pr.yaml");
const overlayDir = "hack/public-overlay/";

const SKIP_DIRS = new Set(["node_modules", "dist", "coverage"]);
const SOURCE_FILE = /\.[cm]?[jt]sx?$/;
const PARENT_LITERAL = /["'`]((?:\.\.\/)+[^"'`\s]*)["'`]/g;

function sourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    if (SKIP_DIRS.has(entry)) continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) out.push(...sourceFiles(full));
    else if (SOURCE_FILE.test(entry)) out.push(full);
  }
  return out;
}

function repoPath(abs: string): string {
  return relative(repoRoot, abs).split(sep).join("/");
}

/** outsideReads maps each repository path outside frontend/ to the frontend files that name it. */
function outsideReads(): Map<string, string[]> {
  const reads = new Map<string, string[]>();
  for (const file of sourceFiles(frontendRoot)) {
    for (const m of readFileSync(file, "utf8").matchAll(PARENT_LITERAL)) {
      const target = resolve(dirname(file), m[1]);
      if (target === frontendRoot || target.startsWith(frontendRoot + sep)) continue;
      // A path outside the repository or a missing path is not a CI input.
      if (!target.startsWith(repoRoot + sep) || !existsSync(target)) continue;
      const path = repoPath(target);
      reads.set(path, [...(reads.get(path) ?? []), repoPath(file)]);
    }
  }
  return reads;
}

interface Workflow {
  jobs: {
    changes: {
      outputs: Record<string, string>;
      steps: { id?: string; with?: { filters?: string } }[];
    };
    "frontend-test": { if: string };
  };
}

function loadWorkflow(file: string): { workflow: Workflow; rules: string[] } {
  const workflow = parseYAML(readFileSync(file, "utf8")) as Workflow;
  const step = workflow.jobs.changes.steps.find((s) => s.id === "paths");
  const filters = parseYAML(step?.with?.filters ?? "") as Record<string, string[]>;
  return { workflow, rules: [...(filters.frontend ?? []), ...(filters.frontendextra ?? [])] };
}

/** covers accepts an exact path or a "<dir>/**" rule. */
function covers(rule: string, path: string): boolean {
  if (rule.endsWith("/**")) {
    const dir = rule.slice(0, -3);
    return path === dir || path.startsWith(`${dir}/`);
  }
  return rule === path;
}

const reads = outsideReads();

const copies = [
  { name: ".github/workflows/pr.yaml", file: liveWorkflow, skip: (_path: string) => false },
  { name: `${overlayDir}.github/workflows/pr.yaml`, file: overlayWorkflow, skip: (path: string) => path.startsWith(overlayDir) },
].filter((c) => existsSync(c.file));

describe("Frontend test path filter", () => {
  it("finds the workflow read of this test", () => {
    expect(copies[0]?.file).toBe(liveWorkflow);
    expect(reads.get(".github/workflows/pr.yaml")).toContain("frontend/src/ciPathFilter.test.ts");
  });

  for (const copy of copies) {
    const { workflow, rules } = loadWorkflow(copy.file);

    it(`${copy.name} names each outside file that a frontend test reads`, () => {
      const missing = [...reads]
        .filter(([path]) => !copy.skip(path) && !rules.some((rule) => covers(rule, path)))
        .map(([path, readers]) => `${path} (read by ${readers.join(", ")})`);
      expect(missing, "add each path to the frontendextra rule").toEqual([]);
    });

    it(`${copy.name} runs the Frontend test job on the frontendextra paths`, () => {
      const output = workflow.jobs.changes.outputs.frontendtest;
      expect(output).toContain("steps.paths.outputs.frontend == 'true'");
      expect(output).toContain("steps.paths.outputs.frontendextra == 'true'");
      expect(workflow.jobs["frontend-test"].if).toBe("needs.changes.outputs.frontendtest == 'true'");
    });
  }
});
