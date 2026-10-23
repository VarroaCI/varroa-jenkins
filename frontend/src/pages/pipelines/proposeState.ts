// proposeState turns a proposal session into the view that the Tenant home card
// shows. The rules are pure, so the card and its tests share one answer.
import type { AssistantApproval, AssistantSessionDetail } from "../../types/assistant";
import { SKIP_LABELS, STATE_LABELS } from "../../components/assistant/labels";

/** The tool that opens the pull request. */
export const PROPOSE_TOOL = "open_pull_request";

/** The arguments of an open_pull_request approval, read tolerantly. */
export interface PrArgs {
  repo: string;
  branch: string;
  title: string;
  body: string;
  files: { path: string; content: string }[];
}

/** The view of a proposal. Each kind names one state of the card. */
export type ProposeView =
  | { kind: "form" }
  | { kind: "reading"; steps: number }
  | { kind: "waiting"; tool: string }
  | { kind: "skipped"; reason: string }
  | { kind: "ready"; approval: AssistantApproval; args: PrArgs }
  | { kind: "opened"; url: string; number: number }
  | { kind: "failed"; text: string }
  | { kind: "noProposal" }
  | { kind: "denied" }
  | { kind: "expired" };

const str = (v: unknown): string => (typeof v === "string" ? v : "");

/** parsePrArgs reads the arguments of a proposal. Missing fields read as empty. */
export function parsePrArgs(a: unknown): PrArgs {
  const o = a && typeof a === "object" ? (a as Record<string, unknown>) : {};
  const files = Array.isArray(o.files)
    ? o.files
        .filter((f): f is { path: string; content: string } =>
          !!f && typeof f === "object" && typeof (f as { path?: unknown }).path === "string" &&
          typeof (f as { content?: unknown }).content === "string")
        .map((f) => ({ path: f.path, content: f.content }))
    : [];
  return { repo: str(o.repo), branch: str(o.branch), title: str(o.title), body: str(o.body), files };
}

/** parsePrResult reads the result of an opened pull request. It answers null for any other text. */
export function parsePrResult(r: string | null): { url: string; number: number } | null {
  if (!r) return null;
  let v: unknown;
  try {
    v = JSON.parse(r);
  } catch {
    return null;
  }
  if (!v || typeof v !== "object") return null;
  const o = v as { url?: unknown; number?: unknown };
  if (typeof o.url !== "string") return null;
  return { url: o.url, number: typeof o.number === "number" ? o.number : 0 };
}

const byRequestedAt = (a: AssistantApproval, b: AssistantApproval) =>
  Date.parse(a.requestedAt) - Date.parse(b.requestedAt);

/** proposeView applies the rules of the card, in order. */
export function proposeView(d: AssistantSessionDetail | undefined): ProposeView {
  if (!d) return { kind: "form" };
  const prs = d.approvals.filter((a) => a.tool === PROPOSE_TOOL).sort(byRequestedAt);
  const newest = prs.length > 0 ? prs[prs.length - 1] : undefined;
  const state = d.session.state;

  // 1. An opened pull request wins over the session state.
  const opened = [...prs].reverse().find((a) => a.state === "approved" && parsePrResult(a.result) !== null);
  if (opened) {
    const r = parsePrResult(opened.result);
    if (r) return { kind: "opened", url: r.url, number: r.number };
  }

  // 2. A session that still runs or waits.
  if (state === "running") return { kind: "reading", steps: d.session.steps };
  if (state === "waiting_approval") {
    if (newest && newest.state === "pending") {
      return { kind: "ready", approval: newest, args: parsePrArgs(newest.arguments) };
    }
    const pending = d.approvals.find((a) => a.state === "pending");
    return { kind: "waiting", tool: pending?.tool ?? "a tool" };
  }
  if (state === "skipped") {
    const reason = d.session.skipReason ? SKIP_LABELS[d.session.skipReason] : undefined;
    return { kind: "skipped", reason: reason ?? "The assistant did not run." };
  }
  if (state === "expired") return { kind: "expired" };

  // 3. A session that has ended: the newest proposal decides.
  if (newest?.state === "approved") {
    return { kind: "failed", text: newest.result ?? "The pull request was not opened." };
  }
  if (newest?.state === "denied") return { kind: "denied" };
  if (newest?.state === "expired") return { kind: "expired" };
  if (state === "failed" || state === "cancelled") {
    return { kind: "failed", text: STATE_LABELS[state] };
  }
  return { kind: "noProposal" };
}
