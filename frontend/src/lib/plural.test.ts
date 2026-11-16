import { describe, it, expect } from "vitest";
import { plural } from "./plural";

describe("plural", () => {
  it("uses the singular noun for one", () => {
    expect(plural(1, "run", "runs")).toBe("1 run");
  });

  it("uses the plural noun for zero", () => {
    expect(plural(0, "run", "runs")).toBe("0 runs");
  });

  it("uses the plural noun for two", () => {
    expect(plural(2, "run", "runs")).toBe("2 runs");
  });
});
