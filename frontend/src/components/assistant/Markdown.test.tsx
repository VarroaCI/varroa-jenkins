import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { Markdown } from "./Markdown";

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
});
