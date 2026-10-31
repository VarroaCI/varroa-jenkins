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

const css = fs.readFileSync(path.join(__dirname, "Markdown.module.css"), "utf-8");

describe("assistant Markdown layout", () => {
  it("gives every block after the first its space back", () => {
    expect(ruleBody(css, ".markdown > * + *")).toMatch(/margin-top:/);
  });

  it("separates a heading from the block before it", () => {
    expect(ruleBody(css, ".markdown > * + :is(h1, h2, h3, h4, h5, h6)")).toMatch(/margin-top:/);
  });

  it("indents nested lists", () => {
    expect(ruleBody(css, ".markdown :is(ul, ol)")).toMatch(/padding-left:/);
  });

  it("lets a table scroll inside its wrapper", () => {
    expect(ruleBody(css, ".tableScroll")).toMatch(/overflow-x:\s*auto/);
  });

  it("keeps the code block background on the pre, not the code", () => {
    expect(ruleBody(css, ".markdown pre code")).toMatch(/background:\s*none/);
  });
});
