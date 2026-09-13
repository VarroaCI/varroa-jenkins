/**
 * jobGlobMatches reports whether a folder-relative pattern matches a
 * folder-relative job path. The grammar is exactly "*", "**", and "?",
 * anchored at both ends: "*" matches a run holding no "/", "**" matches any
 * run, "?" matches one Unicode code point, and every other character
 * matches itself. It mirrors internal/rbac/jobglob.Match. The "u" flag on
 * the compiled RegExp makes "?" match one code point, the same unit Go's
 * `regexp` package matches with "." — without it a JavaScript "." matches
 * one UTF-16 code unit, splitting a character outside the Basic
 * Multilingual Plane in two.
 *
 * This module is the browser's only glob evaluator. A second one would
 * drift from the Go evaluator that decides the same question on the server.
 */
export function jobGlobMatches(pattern: string, path: string): boolean {
  let re = "";
  for (let i = 0; i < pattern.length; i++) {
    if (pattern.startsWith("**", i)) {
      re += ".*";
      i += 1;
    } else if (pattern[i] === "*") {
      re += "[^/]*";
    } else if (pattern[i] === "?") {
      re += ".";
    } else {
      re += pattern[i].replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    }
  }
  return new RegExp(`^${re}$`, "u").test(path);
}
