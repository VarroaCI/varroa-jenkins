import { stripTenantFolder } from "./jobPath";

/**
 * Title formatters for the pipelines pages. Every browser tab title for a
 * pipelines page comes from this file. The state leads a build title, so a
 * narrow tab keeps the state when it cuts the end of the title.
 */

/** relativeJob returns the job path without its tenant folder. */
export function relativeJob(tenantFolder: string, jobPath: string): string {
  return stripTenantFolder(tenantFolder ?? "", jobPath) || jobPath;
}

export function indexTitle(): string {
  return "Pipelines";
}

export function tenantTitle(tenant: string): string {
  return `${tenant} · Pipelines`;
}

export function jobTitle(tenant: string, tenantFolder: string, jobPath: string): string {
  return `${relativeJob(tenantFolder, jobPath)} · ${tenant}`;
}

export function buildTitle(
  tenant: string,
  run: { buildNumber?: number; tenantFolder: string; jobPath: string },
  stateLabel: string,
): string {
  return `${stateLabel} · #${run.buildNumber ?? "?"} ${relativeJob(run.tenantFolder, run.jobPath)} · ${tenant}`;
}

export function notStartedTitle(tenant: string, tenantFolder: string, jobPath: string): string {
  return `Not started · ${relativeJob(tenantFolder, jobPath)} · ${tenant}`;
}

export function testCaseTitle(tenant: string, job: string, testName: string): string {
  return `${testName} · ${job} · ${tenant}`;
}
