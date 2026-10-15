import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { breakablePath } from "./breakablePath";

function renderPath(text: string) {
  return render(<span data-testid="path">{breakablePath(text)}</span>).getByTestId("path");
}

describe("breakablePath", () => {
  it("keeps the text content unchanged", () => {
    const text = "refs/remotes/origin/smoke-cells";
    expect(renderPath(text).textContent).toBe(text);
  });

  it("inserts one break after each slash, dot, and underscore", () => {
    const el = renderPath("a/b.c_d");
    expect(el.querySelectorAll("wbr")).toHaveLength(3);
    expect(Array.from(el.childNodes).map((n) => n.nodeName)).toEqual([
      "#text",
      "WBR",
      "#text",
      "WBR",
      "#text",
      "WBR",
      "#text",
    ]);
  });

  it("inserts no break after a trailing separator", () => {
    const el = renderPath("pkg/");
    expect(el.querySelectorAll("wbr")).toHaveLength(0);
    expect(el.textContent).toBe("pkg/");
  });

  it("returns the plain string when no separator is present", () => {
    expect(breakablePath("checkout")).toBe("checkout");
  });
});
