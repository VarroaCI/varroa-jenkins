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
