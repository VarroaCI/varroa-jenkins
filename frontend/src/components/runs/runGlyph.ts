/**
 * RUN_STATUSES lists every run status the API answers. It mirrors the
 * runs.status constants in internal/recordstore/ingest_common.go, and
 * runGlyph.test.ts fails when the two lists differ.
 */
export const RUN_STATUSES = ["QUEUED", "RUNNING", "COMPLETED", "DUPLICATE"] as const;

/** RunStatus is one run status the API answers. */
export type RunStatus = (typeof RUN_STATUSES)[number];

/** RunGlyph names the state a run shows as its status mark. */
export type RunGlyph =
  | "success"
  | "failure"
  | "unstable"
  | "aborted"
  | "interrupted"
  | "running"
  | "queued"
  | "waiting"
  | "duplicate"
  | "other";

/** RunState is the part of a run that sets its status mark. */
export interface RunState {
  status: string;
  result?: string;
  pendingInput?: boolean;
}

/**
 * STATUS_GLYPH gives each status its mark. A COMPLETED run takes the mark
 * of its result, so it maps to null. A DUPLICATE run keeps its own mark
 * even when it stores a result.
 */
const STATUS_GLYPH: Record<RunStatus, RunGlyph | null> = {
  QUEUED: "queued",
  RUNNING: "running",
  COMPLETED: null,
  DUPLICATE: "duplicate",
};

/** TERMINAL_STATUS is true for a status that no later build event changes. */
const TERMINAL_STATUS: Record<RunStatus, boolean> = {
  QUEUED: false,
  RUNNING: false,
  COMPLETED: true,
  DUPLICATE: true,
};

function isRunStatus(status: string): status is RunStatus {
  return (RUN_STATUSES as readonly string[]).includes(status);
}

/**
 * isTerminalRunStatus is true for COMPLETED and DUPLICATE. A terminal run
 * shows no spinner and no Cancel, and a page stops polling its record. An
 * unknown status is not terminal.
 */
export function isTerminalRunStatus(status: string): boolean {
  return isRunStatus(status) && TERMINAL_STATUS[status];
}

/** DUPLICATE_RUN_NOTE explains a DUPLICATE run where a surface has room for a sentence. */
export const DUPLICATE_RUN_NOTE =
  "Another run of the same trigger or revision was already live. Varroa stopped this run.";

/** runGlyph answers the status mark of a run. A pending input outranks the status. */
export function runGlyph(r: RunState): RunGlyph {
  if (r.pendingInput) return "waiting";
  const byStatus = isRunStatus(r.status) ? STATUS_GLYPH[r.status] : null;
  if (byStatus) return byStatus;
  switch (r.result) {
    case "SUCCESS":
      return "success";
    case "FAILURE":
    case "TRIGGER_FAILED":
      return "failure";
    case "UNSTABLE":
      return "unstable";
    case "ABORTED":
      return "aborted";
    case "INTERRUPTED":
      return "interrupted";
    default:
      return "other";
  }
}

/** runGlyphLabel names a glyph for assistive technology and for the reader. */
export function runGlyphLabel(g: RunGlyph): string {
  switch (g) {
    case "success":
      return "Success";
    case "failure":
      return "Failure";
    case "unstable":
      return "Unstable";
    case "aborted":
      return "Aborted";
    case "interrupted":
      return "Interrupted";
    case "running":
      return "Running";
    case "queued":
      return "Queued";
    case "waiting":
      return "Waiting for input";
    case "duplicate":
      return "Duplicate";
    default:
      return "Other";
  }
}
