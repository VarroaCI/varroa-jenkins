import fs from "fs";
import path from "path";
import { describe, it, expect } from "vitest";

/** ruleBody returns the declarations of the first rule whose selector is `selector`. */
function ruleBody(css: string, selector: string): string {
  const start = css.indexOf(`${selector} {`);
  if (start === -1) throw new Error(`no rule for ${selector}`);
  const open = css.indexOf("{", start);
  const close = css.indexOf("}", open);
  return css.slice(open + 1, close);
}

const read = (file: string) => fs.readFileSync(path.join(__dirname, file), "utf-8");

describe("pipelines table layout", () => {
  it("never wraps a job name anywhere, so the name keeps a usable width", () => {
    const body = ruleBody(read("JobTree.module.css"), ".nodeName");
    expect(body).not.toMatch(/overflow-wrap:\s*anywhere/);
    expect(body).toMatch(/overflow-wrap:\s*break-word/);
  });

  it("defines the horizontal scroll wrapper on the tenant home and the build page", () => {
    expect(read("TenantHome.module.css")).toMatch(/\.tableScroll\s*\{/);
    expect(read("BuildPage.module.css")).toMatch(/\.tableScroll\s*\{/);
  });

  it("the pipelines home stacks at phone width", () => {
    const page = read("PipelinesIndex.module.css");
    expect(page).toMatch(/@media \(max-width: 639\.98px\)\s*\{[\s\S]*?\.body\s*\{\s*grid-template-columns:\s*minmax\(0,\s*1fr\);/);
    expect(read("TenantRail.module.css")).toMatch(/@media \(max-width: 639\.98px\)\s*\{[\s\S]*?overflow-x:\s*auto;/);
  });
});
