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
  | "other";

/** RunState is the part of a run that sets its status mark. */
export interface RunState {
  status: string;
  result?: string;
  pendingInput?: boolean;
}

/** runGlyph answers the status mark of a run. A pending input outranks the status. */
export function runGlyph(r: RunState): RunGlyph {
  if (r.pendingInput) return "waiting";
  if (r.status === "QUEUED") return "queued";
  if (r.status === "RUNNING") return "running";
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
    default:
      return "Other";
  }
}
