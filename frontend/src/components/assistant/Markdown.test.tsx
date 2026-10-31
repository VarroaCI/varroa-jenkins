import fs from "fs";
import path from "path";
import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { Markdown } from "./Markdown";

const SAMPLE = fs.readFileSync(path.join(__dirname, "testdata", "answer-sample.md"), "utf-8");

describe("Markdown", () => {
  it("renders text and code without raw HTML", () => {
    const { container } = render(<Markdown text={"**bold** and `code`\n\n<script>alert(1)</script>"} />);
    expect(container.querySelector("strong")?.textContent).toBe("bold");
    expect(container.querySelector("code")?.textContent).toBe("code");
    expect(container.querySelector("script")).toBeNull();
  });

  it("drops images and keeps the link text", () => {
    const { container } = render(<Markdown text={"![pic](https://x.test/a.png) see [docs](https://varroa.dev)"} />);
    expect(container.querySelector("img")).toBeNull();
    expect(container.textContent).toContain("docs");
  });

  it("opens links in a new tab without an opener", () => {
    const { container } = render(<Markdown text={"[docs](https://varroa.dev)"} />);
    const a = container.querySelector("a");
    expect(a?.getAttribute("target")).toBe("_blank");
    expect(a?.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("drops an onclick handler written as text", () => {
    const { container } = render(<Markdown text={'<a href="javascript:alert(1)" onclick="x()">bad</a>'} />);
    expect(container.querySelector("[onclick]")).toBeNull();
    expect(container.querySelector("a[href^='javascript']")).toBeNull();
  });

  it("renders a GFM table inside a focusable scroll wrapper", () => {
    const { container } = render(<Markdown text={"| a | b |\n| - | - |\n| 1 | 2 |"} />);
    const table = container.querySelector("table");
    const wrap = table?.parentElement;
    expect(wrap?.getAttribute("tabindex")).toBe("0");
    expect(wrap?.getAttribute("role")).toBe("region");
  });

  it("keeps a single tilde as text and strikes a double tilde", () => {
    const { container } = render(<Markdown text={"~~old~~ and ~3 min to ~5 min"} />);
    expect(container.querySelector("del")?.textContent).toBe("old");
    expect(container.textContent).toContain("~3 min to ~5 min");
  });

  it("renders a task list as disabled checkboxes", () => {
    const { container } = render(<Markdown text={"- [x] done\n- [ ] open"} />);
    const boxes = container.querySelectorAll("input[type='checkbox']");
    expect(boxes).toHaveLength(2);
    boxes.forEach((b) => expect((b as HTMLInputElement).disabled).toBe(true));
  });

  it("autolinks a bare URL into a new tab", () => {
    const { container } = render(<Markdown text={"see https://varroa.dev/docs for more"} />);
    const a = container.querySelector("a");
    expect(a?.getAttribute("href")).toBe("https://varroa.dev/docs");
    expect(a?.getAttribute("target")).toBe("_blank");
  });

  it("keeps a footnote link in the same tab", () => {
    const { container } = render(<Markdown text={"A claim.[^1]\n\n[^1]: The source."} />);
    const a = container.querySelector("a[href^='#']");
    expect(a).not.toBeNull();
    expect(a?.hasAttribute("target")).toBe(false);
  });

  it("renders the sample answer as headings and nested lists", () => {
    const { container } = render(<Markdown text={SAMPLE} />);
    const headings = Array.from(container.querySelectorAll("h3")).map((h) => h.textContent);
    expect(headings).toEqual(["Build Overview", "Timeline and Stages", "Build Environment", "Notes"]);
    const ordered = container.querySelector("ol");
    const items = Array.from(ordered?.querySelectorAll(":scope > li") ?? []);
    expect(items).toHaveLength(14);
    items.forEach((li) => expect(li.querySelector("ul")).not.toBeNull());
    expect(container.textContent).not.toContain("###");
    expect(container.querySelector("del")).toBeNull();
  });
});
