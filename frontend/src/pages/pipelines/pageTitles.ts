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

export function teamTitle(team: string): string {
  return `${team} · Pipelines`;
}

export function jobTitle(team: string, tenantFolder: string, jobPath: string): string {
  return `${relativeJob(tenantFolder, jobPath)} · ${team}`;
}

export function buildTitle(
  team: string,
  run: { buildNumber?: number; tenantFolder: string; jobPath: string },
  stateLabel: string,
): string {
  return `${stateLabel} · #${run.buildNumber ?? "?"} ${relativeJob(run.tenantFolder, run.jobPath)} · ${team}`;
}

export function notStartedTitle(team: string, tenantFolder: string, jobPath: string): string {
  return `Not started · ${relativeJob(tenantFolder, jobPath)} · ${team}`;
}

export function testCaseTitle(team: string, job: string, testName: string): string {
  return `${testName} · ${job} · ${team}`;
}
