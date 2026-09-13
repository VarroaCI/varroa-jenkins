import { describe, it, expect } from "vitest";
import { jobGlobMatches } from "./jobglob";

// This corpus carries the same cases, under the same names, as TestMatch in
// internal/rbac/jobglob/jobglob_test.go. The two evaluators decide one
// question on two sides of the wire. A grammar change that updates one side
// and not the other fails one of the two suites.
describe("jobGlobMatches", () => {
  const cases: Array<{ name: string; pattern: string; path: string; want: boolean }> = [
    { name: "literal match", pattern: "checkout-api", path: "checkout-api", want: true },
    { name: "literal mismatch", pattern: "checkout-api", path: "checkout-api-2", want: false },

    { name: "single star matches one segment", pattern: "release/*", path: "release/eu", want: true },
    {
      name: "single star does not cross a slash",
      pattern: "release/*",
      path: "release/eu/deploy",
      want: false,
    },

    { name: "double star matches one segment", pattern: "release/**", path: "release/eu", want: true },
    {
      name: "double star crosses a slash",
      pattern: "release/**",
      path: "release/eu/deploy",
      want: true,
    },

    { name: "question mark matches one character", pattern: "eu?", path: "eu1", want: true },
    { name: "question mark requires exactly one character", pattern: "eu?", path: "eu", want: false },
    {
      name: "question mark matches a slash",
      pattern: "eu?deploy",
      path: "eu/deploy",
      want: true,
    },

    { name: "regexp metacharacter matches itself", pattern: "v1.2+build", path: "v1.2+build", want: true },
    {
      name: "regexp metacharacter does not act as a wildcard",
      pattern: "v1.2+build",
      path: "v1x2xbuild",
      want: false,
    },
  ];

  for (const tc of cases) {
    it(tc.name, () => {
      expect(jobGlobMatches(tc.pattern, tc.path)).toBe(tc.want);
    });
  }

  // The browser needs one case the Go corpus does not: JavaScript matches
  // UTF-16 code units by default, so the "u" flag is what makes "?" match
  // one code point, the unit Go's regexp package matches with ".".
  it("question mark matches one code point outside the Basic Multilingual Plane", () => {
    expect(jobGlobMatches("?", "\u{1F600}")).toBe(true);
    expect(jobGlobMatches("?", "\u{1F600}\u{1F600}")).toBe(false);
  });
});
