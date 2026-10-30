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

describe("wide table layout", () => {
  it("the shared wrapper scrolls and draws an edge cue", () => {
    const body = ruleBody(read("tables.module.css"), ".tableScroll");
    expect(body).toMatch(/overflow-x:\s*auto/);
    expect(body).toMatch(/local/);
    expect(body).toMatch(/radial-gradient/);
  });

  it("a wrapped data table stays a table under the phone breakpoint", () => {
    expect(read("tables.module.css")).toMatch(/\.tableScroll \.dataTable\s*\{[^}]*display:\s*table/);
  });

  it("each wide-table page composes the shared wrapper", () => {
    expect(read("Cells.module.css")).toMatch(/composes:\s*tableScroll from/);
    expect(read("CellTab.module.css")).toMatch(/composes:\s*tableScroll from/);
    expect(read("Agents.module.css")).toMatch(/composes:\s*tableScroll from/);
    expect(read("pipelines/PipelinePage.module.css")).toMatch(/composes:\s*tableScroll from/);
  });

  it("wide tables set a minimum width", () => {
    expect(ruleBody(read("Cells.module.css"), ".table")).toMatch(/min-width:/);
    expect(ruleBody(read("CellTab.module.css"), ".wideTable")).toMatch(/min-width:/);
    expect(ruleBody(read("Agents.module.css"), ".table")).toMatch(/min-width:/);
    expect(ruleBody(read("pipelines/PipelinePage.module.css"), ".buildsTable")).toMatch(/min-width:/);
    expect(ruleBody(read("pipelines/TenantHome.module.css"), ".jobsTable")).toMatch(/min-width:/);
  });

  it("the delivery pipeline stacks at phone width", () => {
    const css = read("../components/DeliveryPipeline.module.css");
    const phone = css.slice(css.indexOf("@media (max-width: 639.98px)"));
    expect(css).toMatch(/max-width:\s*639\.98px/);
    expect(phone).toMatch(/\.pipe\s*\{[^}]*grid-template-columns:\s*minmax\(0,\s*1fr\)/);
  });

  it("the PR chip truncates", () => {
    expect(ruleBody(read("pipelines/PipelinePage.module.css"), ".prChip")).toMatch(/text-overflow:\s*ellipsis/);
  });

  it("bundle input meta wraps", () => {
    expect(ruleBody(read("ComposedBundleDetail.module.css"), ".itemMeta")).toMatch(/overflow-wrap:\s*anywhere/);
  });

  it("catalog card footer wraps", () => {
    expect(ruleBody(read("CatalogBrowser.module.css"), ".cardFooter")).toMatch(/flex-wrap:\s*wrap/);
  });
});
