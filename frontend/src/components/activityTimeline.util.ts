import type { ActivityEvent } from "../types";

// ── Constants ──────────────────────────────────────────────────────────────

export const GROUP_GAP_MS = 120_000; // 2 minutes
export const GROUP_MAX = 200;
export const MAX_BUFFER = 2000;
export const NEW_PILL_CAP = 999;

// ── Types ──────────────────────────────────────────────────────────────────

export type Lane = "control" | "builds";

export type RenderRow =
  | { kind: "event"; event: ActivityEvent }
  | {
      kind: "group";
      groupId: string;
      groupKey: string;
      members: ActivityEvent[];
      newest: ActivityEvent;
      breakdown: Record<string, number>;
    };

// ── Lane classification ────────────────────────────────────────────────────

export function laneFor(e: ActivityEvent): Lane {
  // 1. operator → control
  if (e.source === "operator") return "control";
  // 2. mite → control
  if (e.source === "mite") return "control";
  // 3. user → control
  if (e.source === "user") return "control";
  // 4. jenkins + buildNumber != null → builds
  if (e.source === "jenkins" && e.buildNumber != null) return "builds";
  // 5. jenkins + no buildNumber → control
  if (e.source === "jenkins" && e.buildNumber == null) return "control";
  // 6. any other / unknown source → control (fail-safe)
  return "control";
}

// ─── Timestamp helper ──────────────────────────────────────────────────────

export function tsMs(e: ActivityEvent): number {
  return new Date(e.timestamp).getTime();
}

// ── Event key for dedup ────────────────────────────────────────────────────

const SEP = "\u001F";

export function eventKey(e: ActivityEvent): string {
  return [
    e.cluster ?? "",
    e.timestamp,
    e.source,
    e.type,
    e.namespace ?? "",
    e.controller ?? "",
    e.buildNumber ?? "",
    e.message,
  ].join(SEP);
}

// ── Merge + dedup ──────────────────────────────────────────────────────────

export function mergeEvents(
  buffer: ActivityEvent[],
  incoming: ActivityEvent[],
  cap = MAX_BUFFER,
): ActivityEvent[] {
  const existingKeys = new Set<string>();
  for (const ev of buffer) {
    existingKeys.add(eventKey(ev));
  }

  const merged = [...buffer];

  for (const ev of incoming) {
    if (!existingKeys.has(eventKey(ev))) {
      existingKeys.add(eventKey(ev));
      merged.push(ev);
    }
  }

  // Sort newest-first by timestamp (stable for ties)
  merged.sort((a, b) => tsMs(b) - tsMs(a));

  // Truncate to cap, dropping oldest
  if (merged.length > cap) {
    return merged.slice(0, cap);
  }
  return merged;
}

// ── Result icon/style map ──────────────────────────────────────────────────

export function resultMeta(
  e: ActivityEvent,
): { icon: string; style: string } {
  const r = e.result?.toUpperCase();
  switch (r) {
    case "SUCCESS":
      return { icon: "✔", style: "softOk" };
    case "FAILURE":
      return { icon: "✘", style: "softBad" };
    case "UNSTABLE":
      return { icon: "▲", style: "softWarn" };
    case "ABORTED":
      return { icon: "⊘", style: "softMuted" };
    case "NOT_BUILT":
      return { icon: "◌", style: "softMuted" };
    default:
      // absent / null → running
      return { icon: "◷", style: "softInfo" };
  }
}

// ── Group key (namespace + " " + controller) ───────────────────────────────

export function groupKey(e: ActivityEvent): string {
  return (e.cluster ?? "") + " " + (e.namespace ?? "") + " " + (e.controller ?? "");
}

// ── Grouping (single pass, newest-first) ───────────────────────────────────

export function groupEvents(rows: ActivityEvent[]): RenderRow[] {
  const result: RenderRow[] = [];

  // Current open build group state
  let openGroupKey: string | null = null;
  let openGroupMembers: ActivityEvent[] = [];
  let openGroupOldestTs: string | null = null;

  function flushGroup() {
    if (openGroupMembers.length === 0) return;

    if (openGroupMembers.length === 1) {
      // Singleton → plain event row
      result.push({ kind: "event", event: openGroupMembers[0] });
    } else {
      const youngest = openGroupMembers[0]; // first appended = newest
      const oldest = openGroupMembers[openGroupMembers.length - 1]; // last = oldest
      const gk = openGroupKey!;
      const gid = gk + "@" + oldest.timestamp;

      // Build breakdown
      const breakdown: Record<string, number> = {};
      for (const m of openGroupMembers) {
        const key = m.result?.toUpperCase() ?? "running";
        breakdown[key] = (breakdown[key] ?? 0) + 1;
      }

      result.push({
        kind: "group",
        groupId: gid,
        groupKey: gk,
        members: openGroupMembers,
        newest: youngest,
        breakdown,
      });
    }

    openGroupKey = null;
    openGroupMembers = [];
    openGroupOldestTs = null;
  }

  for (const row of rows) {
    if (laneFor(row) === "control") {
      // Control-plane event flushes any open group
      flushGroup();
      result.push({ kind: "event", event: row });
      continue;
    }

    // Build lane event
    const gk = groupKey(row);

    if (
      openGroupKey === gk &&
      openGroupMembers.length < GROUP_MAX &&
      openGroupOldestTs !== null &&
      Math.abs(tsMs(row) - tsMs(openGroupMembers[openGroupMembers.length - 1])) <= GROUP_GAP_MS
    ) {
      // Join current open group
      openGroupMembers.push(row);
    } else {
      // Flush and start new group
      flushGroup();
      openGroupKey = gk;
      openGroupMembers = [row];
      openGroupOldestTs = row.timestamp;
    }
  }

  // Flush trailing open group
  flushGroup();

  return result;
}

// ── Filter predicates ──────────────────────────────────────────────────────

export function passesScope(
  e: ActivityEvent,
  scope?: { cluster?: string; namespace: string; name: string },
): boolean {
  if (!scope) return true;
  if (scope.cluster && e.cluster !== scope.cluster) return false;
  return e.namespace === scope.namespace && e.controller === scope.name;
}

export function passesSelection(
  e: ActivityEvent,
  selection: Set<string>,
): boolean {
  if (selection.size === 0) return true;
  if (e.controller) {
    return selection.has((e.cluster ?? "") + "/" + (e.namespace ?? "") + "/" + e.controller);
  }
  // No controller → only show if __global__ is selected
  return selection.has("__global__");
}

export function passesLane(
  e: ActivityEvent,
  lane: "All" | Lane,
): boolean {
  return lane === "All" || laneFor(e) === lane;
}

export function passesSource(
  e: ActivityEvent,
  source: "All" | string,
): boolean {
  return source === "All" || e.source === source;
}

// ── Source metadata (moved from Activity.tsx) ──────────────────────────────

export const ACTIVITY_TYPE_MAP: Record<string, { icon: string; style: string }> = {
  connected: { icon: "⇡", style: "softInfo" },
  disconnected: { icon: "⇣", style: "softBad" },
  phase: { icon: "◴", style: "softWarn" },
  "reconcile.triggered": { icon: "↻", style: "softInfo" },
  "controller.created": { icon: "➕", style: "softOk" },
  "controller.deleted": { icon: "✗", style: "softBad" },
  "controller.updated": { icon: "✏", style: "softInfo" },
  "provisioning.started": { icon: "▶", style: "softInfo" },
  "provisioning.completed": { icon: "✔", style: "softOk" },
  "provisioning.failed": { icon: "✘", style: "softBad" },
  "group.created": { icon: "➕", style: "softOk" },
  "group.updated": { icon: "✏", style: "softInfo" },
  "group.deleted": { icon: "✗", style: "softBad" },
  "settings.updated": { icon: "⚙", style: "softWarn" },
  login: { icon: "→", style: "softOk" },
  logout: { icon: "←", style: "softWarn" },
  "preferences.updated": { icon: "☰", style: "softInfo" },
  "build.started": { icon: "▶", style: "softInfo" },
  "build.completed": { icon: "⚑", style: "softInfo" },
  "job.created": { icon: "➕", style: "softOk" },
  "job.updated": { icon: "✏", style: "softInfo" },
  "job.deleted": { icon: "✗", style: "softBad" },
};

export const SOURCE_COLORS: Record<string, string> = {
  mite: "var(--ok-soft)",
  operator: "var(--info-soft)",
  user: "var(--accent-soft)",
  jenkins: "var(--warn-soft)",
};

export const SOURCE_LABELS: Record<string, string> = {
  mite: "Mite",
  operator: "Operator",
  user: "User",
  jenkins: "Jenkins",
};

// age renders a relative timestamp. The "heartbeat" variant reports cluster
// heartbeat staleness: seconds up to 120s, minutes up to 120m, then uncapped
// hours, and an explicit "—" for unparseable or future input. The default
// timeline variant is coarser (m/h/d) and lenient on bad input.
export function age(
  ts: string,
  opts: { variant?: "timeline" | "heartbeat"; now?: number } = {},
): string {
  const heartbeat = opts.variant === "heartbeat";
  const parsed = Date.parse(ts);
  const now = opts.now ?? Date.now();
  const diff = now - parsed;
  if (isNaN(parsed) || diff < 0) {
    return heartbeat ? "—" : "just now";
  }
  const secs = Math.floor(diff / 1000);
  if (heartbeat && secs < 120) return `${secs}s ago`;
  const mins = Math.floor(secs / 60);
  if (mins < 1) return "just now";
  if (mins < (heartbeat ? 120 : 60)) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (heartbeat || hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}
