import type { RunTestCase } from "../../types";

export interface TestCaseGroup {
  suite: string;
  /** The class every row of the group shares, when one class does. */
  sharedClassName?: string;
  /** The header text. */
  label: string;
  /** A second header text, when the shared class differs from the suite. */
  secondary?: string;
  rows: RunTestCase[];
}

/** norm maps every character that is not a letter or digit to "_". */
const norm = (s: string) => s.replace(/[^A-Za-z0-9]/g, "_");

/**
 * groupTestCases groups consecutive cases that share a suite. It never
 * reorders a row. A suite can head two groups when its failed cases come
 * before its skipped cases.
 */
export function groupTestCases(cases: RunTestCase[]): { groups: TestCaseGroup[]; showClassColumn: boolean } {
  const runs: { suite: string; rows: RunTestCase[] }[] = [];
  for (const c of cases) {
    const last = runs[runs.length - 1];
    if (last && last.suite === c.suite) last.rows.push(c);
    else runs.push({ suite: c.suite, rows: [c] });
  }
  const groups = runs.map(({ suite, rows }): TestCaseGroup => {
    const first = rows[0].className;
    const sharedClassName = rows.every((r) => r.className === first) ? first : undefined;
    if (sharedClassName !== undefined && norm(sharedClassName) === norm(suite)) {
      return { suite, sharedClassName, label: sharedClassName, rows };
    }
    return { suite, sharedClassName, label: suite, secondary: sharedClassName, rows };
  });
  return { groups, showClassColumn: groups.some((g) => g.sharedClassName === undefined) };
}
