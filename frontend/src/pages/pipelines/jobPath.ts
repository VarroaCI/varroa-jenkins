/**
 * stripTenantFolder returns the folder-relative remainder of jobPath. It
 * mirrors the server's stripFolder (internal/api/authorizer_tenant.go): an
 * empty tenantFolder leaves jobPath unchanged, jobPath equal to tenantFolder
 * strips to the empty string, and jobPath under "<tenantFolder>/" strips
 * that prefix. jobPath is returned unchanged when it does not carry the
 * tenantFolder prefix at all.
 */
export function stripTenantFolder(tenantFolder: string, jobPath: string): string {
  if (tenantFolder === "") return jobPath;
  if (jobPath === tenantFolder) return "";
  const prefix = `${tenantFolder}/`;
  return jobPath.startsWith(prefix) ? jobPath.slice(prefix.length) : jobPath;
}

/**
 * testCaseHref returns the history page path of one test case. The job path
 * and the case key each take one path segment. The case key is opaque and
 * can hold a "/", so it is encoded as one segment and never parsed.
 */
export function testCaseHref(team: string, jobPath: string, caseKey: string): string {
  return `/pipelines/${encodeURIComponent(team)}/${encodeURIComponent(jobPath)}/tests/${encodeURIComponent(caseKey)}`;
}

/** The route state a case link passes. The history page names the class
 * and the test from this state, and never parses the case key. */
export interface TestCaseLinkState {
  className: string;
  name: string;
}
