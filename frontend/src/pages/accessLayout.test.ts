import fs from "fs";
import path from "path";
import { describe, it, expect } from "vitest";

function read(name: string): string {
  return fs.readFileSync(path.join(__dirname, name), "utf-8");
}

/** ruleBody returns the declarations of the first rule for a selector. */
function ruleBody(css: string, selector: string): string {
  const start = css.indexOf(`${selector} {`);
  if (start < 0) throw new Error(`no rule for ${selector}`);
  const open = css.indexOf("{", start);
  const close = css.indexOf("}", open);
  return css.slice(open + 1, close);
}

/** splitTracks splits a track list on whitespace outside parentheses. */
function splitTracks(value: string): string[] {
  const tokens: string[] = [];
  let depth = 0;
  let current = "";
  for (const ch of value.trim()) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (/\s/.test(ch) && depth === 0) {
      if (current) tokens.push(current);
      current = "";
    } else {
      current += ch;
    }
  }
  if (current) tokens.push(current);
  return tokens;
}

function expectFixedTracks(css: string, selector: string) {
  const match = ruleBody(css, selector).match(/grid-template-columns:\s*([^;]+);/);
  expect(match).not.toBeNull();
  for (const track of splitTracks(match![1])) {
    expect(track).toMatch(/^(minmax\(0,.+\)|\d+px)$/);
  }
}

describe("Admin & access layout", () => {
  it("tables.module.css defines a tableScroll wrapper that scrolls horizontally", () => {
    const css = read("tables.module.css");
    expect(ruleBody(css, ".tableScroll")).toMatch(/overflow-x:\s*auto/);
  });

  it("the role binding grid uses fixed-minimum tracks", () => {
    expectFixedTracks(read("RoleBindings.module.css"), ".bindingRow");
  });

  it("the role grid uses fixed-minimum tracks", () => {
    expectFixedTracks(read("Roles.module.css"), ".roleRow");
  });

  it("the Jenkins role form select has no textarea height", () => {
    expect(ruleBody(read("JenkinsRoleForm.module.css"), ".formSelect")).not.toMatch(/min-height/);
  });

  it("the role form keeps the tall box for multi-selects only", () => {
    const css = read("RoleForm.module.css");
    expect(ruleBody(css, ".formSelect")).not.toMatch(/min-height/);
    expect(css).toMatch(/\.formSelect\[multiple\]\s*\{[^}]*min-height:\s*80px/);
  });

  it("the placement column does not wrap", () => {
    expect(ruleBody(read("Tenants.module.css"), ".memberList li")).toMatch(/white-space:\s*nowrap/);
  });
});
