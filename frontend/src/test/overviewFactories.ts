import type {
  ClusterEntry,
  FleetRunRow,
  PipelinesOverview,
  PipelinesOverviewFailingJob,
  PipelinesOverviewHour,
  PipelinesOverviewTenant,
  PipelinesOverviewTotals,
  PipelinesOverviewWaitingInput,
} from "../types";

/** NOW is the fixed clock of the Overview tests. It is noon UTC on 2026-11-14. */
export const NOW = Date.parse("2026-11-14T12:00:00Z");

/** fleetRun builds a finished, failed run of payments / release by default. */
export function fleetRun(over: Partial<FleetRunRow> = {}): FleetRunRow {
  return {
    runId: "run-212",
    status: "COMPLETED",
    result: "FAILURE",
    buildNumber: 212,
    jobPath: "release",
    tenant: "payments",
    tenantFolder: "",
    cluster: "core",
    trigger: { kind: "push", label: "push" },
    pendingInput: false,
    canWrite: true,
    createdAt: "2026-11-14T11:02:00Z",
    startedAt: "2026-11-14T11:02:00Z",
    endedAt: "2026-11-14T11:02:30Z",
    durationMs: 30000,
    actor: "nathan",
    ...over,
  } as FleetRunRow;
}

/** waitingInput builds one run that waits for an approval. */
export function waitingInput(over: Partial<PipelinesOverviewWaitingInput> = {}, run: Partial<FleetRunRow> = {}): PipelinesOverviewWaitingInput {
  return {
    run: fleetRun({
      runId: "run-41",
      tenant: "blockctl",
      jobPath: "deploy",
      buildNumber: 41,
      status: "RUNNING",
      result: undefined,
      pendingInput: true,
      createdAt: "2026-11-14T11:40:00Z",
      startedAt: "2026-11-14T11:48:00Z",
      endedAt: undefined,
      ...run,
    }),
    input: { id: "promote", message: "Promote to production?", parameterCount: 0, submitter: undefined },
    inputCount: 1,
    ...over,
  };
}

/** failingJob builds one failing job, with a passed run before the failure. */
export function failingJob(over: Partial<PipelinesOverviewFailingJob> = {}, run: Partial<FleetRunRow> = {}): PipelinesOverviewFailingJob {
  return {
    run: fleetRun(run),
    stage: "Test",
    testsFailed: 4,
    lastPassed: { runId: "run-209", buildNumber: 209, createdAt: "2026-11-13T11:00:00Z", endedAt: "2026-11-13T11:10:00Z" },
    failuresSincePass: 1,
    ...over,
  };
}

/** clusterEntry builds one cluster of the brood. */
export function clusterEntry(over: Partial<ClusterEntry> = {}): ClusterEntry {
  return {
    name: "member-b",
    healthy: true,
    core: false,
    lastHeartbeat: new Date(NOW - 5 * 60_000).toISOString(),
    operatorVersion: "1.0.0",
    k8sVersion: "1.30.0",
    controllerCount: 2,
    connectedCount: 2,
    state: "active",
    ...over,
  };
}

/** tenantRow builds one rail row of the overview. */
export function tenantRow(name: string, over: Partial<PipelinesOverviewTenant> = {}): PipelinesOverviewTenant {
  return {
    name,
    clusters: ["core"],
    placement: "placed",
    runs24h: 1,
    failed24h: 0,
    running: 0,
    waitingInput: 0,
    locksHeld: 0,
    recent: [],
    ...over,
  };
}

/** totals builds the 24-hour counters. */
export function totals(over: Partial<PipelinesOverviewTotals> = {}): PipelinesOverviewTotals {
  return {
    runs24h: 12,
    runsPrior24h: 9,
    passed: 10,
    failed: 2,
    interrupted: 0,
    finished: 12,
    running: 0,
    queued: 0,
    medianDurationMs: 60000,
    p90DurationMs: 120000,
    ...over,
  };
}

/** hourly builds 24 hourly buckets that end at NOW, with the given failure hours. */
export function hourly(failedHours: number[] = [], runsPerHour = 2): PipelinesOverviewHour[] {
  const start = NOW - 24 * 3600_000;
  return Array.from({ length: 24 }, (_, i) => ({
    start: new Date(start + i * 3600_000).toISOString(),
    runs: runsPerHour,
    failed: failedHours.includes(i) ? 1 : 0,
  }));
}

/** overview builds the body of GET /pipelines/overview. */
export function overview(over: Partial<PipelinesOverview> = {}, tenants: PipelinesOverviewTenant[] = [tenantRow("payments")]): PipelinesOverview {
  return {
    windowStart: new Date(NOW - 24 * 3600_000).toISOString(),
    localCluster: "core",
    clusters: 1,
    canReadFleetLocks: false,
    totals: totals(),
    attention: { failed24h: 0, waitingInput: 0, awaitingCell: 0, locksHeld: 0 },
    hourly: hourly(),
    waitingInputs: [],
    moreWaitingInputs: false,
    failingJobs: [],
    moreFailingJobs: false,
    running: [],
    tenants,
    ...over,
  };
}
