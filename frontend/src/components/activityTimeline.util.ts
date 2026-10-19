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

/** isAgentRow is the one predicate for an agent row: an assistant or team agent source, or an agent actor. */
export function isAgentRow(e: ActivityEvent): boolean {
  return e.source === "assistant" || e.source === "team_agent" || (e.actor ?? "").startsWith("agent:");
}

export function passesSource(
  e: ActivityEvent,
  source: "All" | string,
): boolean {
  if (source === "agents") return isAgentRow(e);
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
  "tenant.webhook_failed": { icon: "✘", style: "softBad" },
  "controller.updated": { icon: "✏", style: "softInfo" },
  "provisioning.started": { icon: "▶", style: "softInfo" },
  "provisioning.completed": { icon: "✔", style: "softOk" },
  "provisioning.failed": { icon: "✘", style: "softBad" },
  "pod.replaced": { icon: "⟳", style: "softWarn" },
  "group.created": { icon: "➕", style: "softOk" },
  "group.deleted": { icon: "✗", style: "softBad" },
  login: { icon: "→", style: "softOk" },
  logout: { icon: "←", style: "softWarn" },
  "preferences.updated": { icon: "☰", style: "softInfo" },
  "build.started": { icon: "▶", style: "softInfo" },
  "build.completed": { icon: "⚑", style: "softInfo" },
  // ── MCP write-auditing types ─────────────────────────────────────────────
  "restart.approved": { icon: "⏻", style: "softWarn" },
  "catalogsource.synced": { icon: "⇄", style: "softInfo" },
  "catalogsource.created": { icon: "➕", style: "softOk" },
  "catalogsource.updated": { icon: "✏", style: "softInfo" },
  "catalogsource.deleted": { icon: "✗", style: "softBad" },
  "composedbundle.created": { icon: "➕", style: "softOk" },
  "composedbundle.updated": { icon: "✏", style: "softInfo" },
  "composedbundle.deleted": { icon: "✗", style: "softBad" },
  "jenkinsrole.created": { icon: "➕", style: "softOk" },
  "jenkinsrole.updated": { icon: "✏", style: "softInfo" },
  "jenkinsrole.deleted": { icon: "✗", style: "softBad" },
  "jenkinsrolebinding.created": { icon: "➕", style: "softOk" },
  "jenkinsrolebinding.updated": { icon: "✏", style: "softInfo" },
  "jenkinsrolebinding.deleted": { icon: "✗", style: "softBad" },
  "varroarole.created": { icon: "➕", style: "softOk" },
  "varroarole.updated": { icon: "✏", style: "softInfo" },
  "varroarole.deleted": { icon: "✗", style: "softBad" },
  "varroarolebinding.created": { icon: "➕", style: "softOk" },
  "varroarolebinding.updated": { icon: "✏", style: "softInfo" },
  "varroarolebinding.deleted": { icon: "✗", style: "softBad" },
  "user.created": { icon: "➕", style: "softOk" },
  "user.updated": { icon: "✏", style: "softInfo" },
  "user.deleted": { icon: "✗", style: "softBad" },
  "versionprofile.created": { icon: "➕", style: "softOk" },
  "versionprofile.updated": { icon: "✏", style: "softInfo" },
  "versionprofile.deleted": { icon: "✗", style: "softBad" },
  "provisioningdefaults.updated": { icon: "✏", style: "softInfo" },
  // ── Auth ─────────────────────────────────────────────────────────────────
  "auth.local.login": { icon: "→", style: "softOk" },
  "auth.local.login_failed": { icon: "⚠", style: "softWarn" },
  // ── Controller lifecycle ─────────────────────────────────────────────────
  "controller.breakglass.granted": { icon: "⚠", style: "softWarn" },
  "controller.hibernated": { icon: "☾", style: "softMuted" },
  "controller.woken": { icon: "☀", style: "softInfo" },
  "controller.restarted": { icon: "⏻", style: "softWarn" },
  "controller.reprovisioned": { icon: "⟳", style: "softWarn" },
  // ── Approvals ────────────────────────────────────────────────────────────
  "deletion.approved": { icon: "✗", style: "softBad" },
  "pluginRoll.approved": { icon: "⇪", style: "softWarn" },
  // ── Cells and tenants ────────────────────────────────────────────────────
  "cell.rolled": { icon: "⟳", style: "softWarn" },
  "tenant.placed": { icon: "⌂", style: "softOk" },
  "tenant.moved": { icon: "⇄", style: "softInfo" },
  "tenant.purge_failed": { icon: "✘", style: "softBad" },
  "cellpool.updated": { icon: "✏", style: "softInfo" },
  // ── Cluster drain ────────────────────────────────────────────────────────
  "cluster.drain.started": { icon: "▼", style: "softWarn" },
  "cluster.drain.completed": { icon: "✔", style: "softOk" },
  "cluster.drain.canceled": { icon: "⊘", style: "softMuted" },
  // ── Brood operations ─────────────────────────────────────────────────────
  "broodop.run.started": { icon: "▶", style: "softInfo" },
  "broodop.run.finished": { icon: "⚑", style: "softInfo" },
  "broodop.target.finished": { icon: "⚑", style: "softInfo" },
  // ── Commands ─────────────────────────────────────────────────────────────
  "command.succeeded": { icon: "✔", style: "softOk" },
  "command.failed": { icon: "✘", style: "softBad" },
  // ── Build claims ─────────────────────────────────────────────────────────
  "build.claim_vetoed": { icon: "⊘", style: "softWarn" },
  "build.claim_abandoned": { icon: "◌", style: "softMuted" },
  "build.claim_expired": { icon: "◌", style: "softMuted" },
  "build.interrupted": { icon: "✘", style: "softBad" },
  "build.scheduled.backstop": { icon: "◷", style: "softInfo" },
  // ── Jenkins items ────────────────────────────────────────────────────────
  "item.created": { icon: "➕", style: "softOk" },
  "item.updated": { icon: "✏", style: "softInfo" },
  "item.deleted": { icon: "✗", style: "softBad" },
  "item.moved": { icon: "⇄", style: "softInfo" },
  // ── Fleet locks ──────────────────────────────────────────────────────────
  "fleetlock.force_released": { icon: "⊘", style: "softWarn" },
  "fleetlock.force_release_unconfirmed": { icon: "⚠", style: "softWarn" },
  // ── Pipelines ────────────────────────────────────────────────────────────
  "pipeline.run_requested": { icon: "▶", style: "softInfo" },
  "pipeline.run_replay_requested": { icon: "↻", style: "softInfo" },
  "pipeline.run_restart_requested": { icon: "⏭", style: "softInfo" },
  "pipeline.run_input_proceeded": { icon: "✔", style: "softOk" },
  "pipeline.run_input_aborted": { icon: "⊘", style: "softWarn" },
  // ── Plugin conflicts ─────────────────────────────────────────────────────
  "pluginConflict.detected": { icon: "⚠", style: "softWarn" },
  "pluginPinConflict.detected": { icon: "⚠", style: "softWarn" },
  // ── Update center ────────────────────────────────────────────────────────
  "updatecenter.plugin.uploaded": { icon: "⇪", style: "softInfo" },
  // ── Upgrade candidates ───────────────────────────────────────────────────
  "upgrade.candidate.created": { icon: "➕", style: "softInfo" },
  "upgrade.candidate.promoted": { icon: "✔", style: "softOk" },
  "upgrade.candidate.superseded": { icon: "⊘", style: "softMuted" },
  "upgrade.preflight.failed": { icon: "✘", style: "softBad" },
  "upgrade.upstream.observed": { icon: "◉", style: "softInfo" },
  // ── Webhooks ─────────────────────────────────────────────────────────────
  "webhook.replay.rejected": { icon: "⊘", style: "softWarn" },
};

export const ACTIVITY_FALLBACK_ICON = "◆";

// activityTypeMeta is the only type-to-icon source for every activity surface.
// An unknown type never renders the "·" marker. It falls back to the auth
// family icon, then to a generic glyph styled by severity.
export function activityTypeMeta(
  e: Pick<ActivityEvent, "type" | "severity">,
): { icon: string; style: string } {
  const exact = ACTIVITY_TYPE_MAP[e.type];
  if (exact) return exact;
  if (e.type.startsWith("auth.")) return { icon: "→", style: "softInfo" };
  const style =
    e.severity === "error" ? "softBad" : e.severity === "warning" ? "softWarn" : "softMuted";
  return { icon: ACTIVITY_FALLBACK_ICON, style };
}

// isActivityEvent reports whether a live frame has the ActivityEvent shape.
// Brood telemetry frames on the same stream lack these fields and must be dropped.
export function isActivityEvent(x: unknown): x is ActivityEvent {
  if (typeof x !== "object" || x === null) return false;
  const v = x as Record<string, unknown>;
  return (
    typeof v.type === "string" &&
    v.type !== "" &&
    typeof v.source === "string" &&
    typeof v.message === "string" &&
    typeof v.timestamp === "string" &&
    RFC3339_TIMESTAMP.test(v.timestamp)
  );
}

// The BFF writes Go time.Time values. Their fractional seconds can carry up to
// nine digits, which Date.parse does not accept in every engine.
const RFC3339_TIMESTAMP =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

export const SOURCE_COLORS: Record<string, string> = {
  mite: "var(--ok-soft)",
  operator: "var(--info-soft)",
  user: "var(--accent-soft)",
  jenkins: "var(--warn-soft)",
  mcp: "var(--honey-soft)",
  assistant: "var(--idle-soft)",
  team_agent: "var(--idle-soft)",
};

export const SOURCE_LABELS: Record<string, string> = {
  mite: "Mite",
  operator: "Operator",
  user: "User",
  jenkins: "Jenkins",
  mcp: "MCP",
  assistant: "Assistant",
  team_agent: "Team agent",
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
