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

/** mediaBody returns the text of the first @media block whose query starts with `query`. */
function mediaBody(css: string, query: string): string {
  const start = css.indexOf(`@media ${query}`);
  if (start === -1) throw new Error(`no media block for ${query}`);
  const open = css.indexOf("{", start);
  let depth = 0;
  for (let i = open; i < css.length; i++) {
    if (css[i] === "{") depth++;
    if (css[i] === "}") {
      depth--;
      if (depth === 0) return css.slice(open + 1, i);
    }
  }
  throw new Error(`unclosed media block for ${query}`);
}

const read = (file: string) => fs.readFileSync(path.join(__dirname, file), "utf-8");

describe("assistant drawer layout", () => {
  it("the drawer is held to the viewport height", () => {
    const body = ruleBody(read("AssistantDrawer.module.css"), ".drawer");
    expect(body).toMatch(/position:\s*sticky/);
    expect(body).toMatch(/height:\s*calc\(100dvh/);
  });

  it("the transcript scrolls inside the drawer", () => {
    const body = ruleBody(read("AssistantDrawer.module.css"), ".transcript");
    expect(body).toMatch(/overflow-y:\s*auto/);
    expect(body).toMatch(/min-height:\s*0/);
  });

  it("the composer textarea can shrink", () => {
    const body = ruleBody(read("AssistantDrawer.module.css"), ".composer textarea");
    expect(body).toMatch(/min-width:\s*0/);
  });

  it("the phone sheet sits above the tab bar", () => {
    const phone = mediaBody(read("AssistantDrawer.module.css"), "(max-width: 639.98px)");
    const z = Number(ruleBody(phone, ".drawer").match(/z-index:\s*(\d+)/)?.[1]);
    const tabBar = Number(ruleBody(read("../TabBar.module.css"), ".tabBar").match(/z-index:\s*(\d+)/)?.[1]);
    expect(z).toBeGreaterThan(tabBar);
  });

  it("the mid-width drawer overlays the page", () => {
    const mid = mediaBody(read("AssistantDrawer.module.css"), "(min-width: 640px) and (max-width: 1023.98px)");
    expect(ruleBody(mid, ".drawer")).toMatch(/position:\s*fixed/);
  });
});
