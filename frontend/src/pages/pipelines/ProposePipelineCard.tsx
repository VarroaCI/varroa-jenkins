// ProposePipelineCard starts a proposal of a first Jenkinsfile for a repository
// and shows its state: reading, waiting for approval, ready to review, opened,
// or ended. The card renders model text as text nodes only, and it writes
// nothing to browser storage.
import { useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  approveAssistantApproval,
  cancelAssistantSession,
  denyAssistantApproval,
  getAssistantSession,
  listAssistantSessions,
  postAssistantMessage,
  proposePipeline,
} from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import { useAssistant } from "../../components/assistant/AssistantProvider";
import { age } from "../../components/activityTimeline.util";
import type { AssistantSessionDetail } from "../../types/assistant";
import { PROPOSE_TOOL, proposeView, type PrArgs, type ProposeView } from "./proposeState";
import styles from "./ProposePipelineCard.module.css";

/** One STE line per refusal code of the proposal route. */
export const PROPOSE_ERROR_LABELS: Record<string, string> = {
  assistant_disabled: "The assistant is not available on this installation.",
  assistant_off: "Your assistant switch is off. Turn it on in your profile.",
  assistant_busy: "You have too many sessions running. Wait for one to end.",
  kill_switch: "The assistant is paused for this installation.",
  repo_url_invalid: "Enter an https repository URL with no port other than 443.",
  data_sharing_too_low: "The data level of this team and provider hides the repository.",
  no_matching_credential: "No team credential covers this forge host.",
  forge_unsupported: "This forge is not supported.",
  forge_auth_failed: "The team credential was refused by the forge.",
  forge_write_denied: "The team credential may not write to this repository.",
  forge_unreachable: "The forge did not answer. Check the network policy.",
  jenkinsfile_exists: "The repository already holds a Jenkinsfile.",
  proposal_branch_exists: "The branch varroa/propose-pipeline exists. Delete the branch or merge its pull request.",
  not_found: "The repository or the team was not found.",
  invalid_request: "The request was not valid.",
  internal: "The proposal failed inside Varroa.",
};

const POLL_MS = 3000;

/** The session states that still change. */
const LIVE_STATES = ["running", "waiting_approval"];

/** errorCode reads the machine code of a refused request, or "". */
function errorCode(err: unknown): string {
  if (!(err instanceof ApiError) || !err.body || typeof err.body !== "object") return "";
  const code = (err.body as { code?: unknown }).code;
  return typeof code === "string" ? code : "";
}

/** ProposePipelineCard shows the proposal form and the state of the proposal. */
export default function ProposePipelineCard({ team }: { team: string }) {
  const assistant = useAssistant();
  const queryClient = useQueryClient();
  const [repo, setRepo] = useState("");
  const [refused, setRefused] = useState("");
  const [startedOver, setStartedOver] = useState(false);
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [denying, setDenying] = useState(false);
  const [reason, setReason] = useState("");
  const [actionError, setActionError] = useState("");

  const sessions = useQuery({
    queryKey: ["propose-session", team],
    queryFn: () => listAssistantSessions({ prompt: "propose-pipeline", team, limit: 1 }),
    enabled: assistant.switchOn,
  });
  const current = sessionId ?? sessions.data?.items[0]?.id ?? null;

  const detail = useQuery({
    queryKey: ["assistant-session", current],
    queryFn: () => getAssistantSession(current!),
    enabled: !!current && !startedOver,
    refetchInterval: (query) => {
      const state = query.state.data?.session.state ?? "";
      return LIVE_STATES.includes(state) ? POLL_MS : false;
    },
  });

  const start = useMutation({
    mutationFn: (url: string) => proposePipeline(team, url),
    onSuccess: (r) => {
      setRefused("");
      setStartedOver(false);
      setSessionId(r.sessionId);
      assistant.openSession(r.sessionId);
    },
    onError: (err) => {
      const code = errorCode(err);
      setRefused(PROPOSE_ERROR_LABELS[code] || (err instanceof ApiError && err.serverMessage) || "The proposal was refused.");
    },
  });

  const refresh = () => {
    if (current) void queryClient.invalidateQueries({ queryKey: ["assistant-session", current] });
    void queryClient.invalidateQueries({ queryKey: ["assistant-approvals"] });
  };

  const decide = async (approve: boolean) => {
    const pending = detail.data?.approvals.find((a) => a.tool === PROPOSE_TOOL && a.state === "pending");
    if (!pending) return;
    setActionError("");
    try {
      if (approve) await approveAssistantApproval(pending.id);
      else await denyAssistantApproval(pending.id, { reason: reason.trim() || undefined });
      assistant.resolved(pending.sessionId);
      setDenying(false);
      setReason("");
      refresh();
    } catch {
      setActionError("The decision was not recorded.");
      refresh();
    }
  };

  const stop = async () => {
    if (!current) return;
    try {
      await cancelAssistantSession(current);
    } catch {
      setActionError("The session was not cancelled.");
    } finally {
      refresh();
    }
  };

  const askAgain = async () => {
    if (!current) return;
    try {
      await postAssistantMessage(current, { message: "Propose the Jenkinsfile again and call open_pull_request." });
    } catch {
      setActionError("The message was not sent.");
    } finally {
      refresh();
    }
  };

  if (!assistant.switchOn) {
    return (
      <p className={styles.line}>
        Turn on the assistant to propose a pipeline. <Link to="/profile?tab=assistant">Open your profile</Link>
      </p>
    );
  }

  const loading = !startedOver && (sessions.isPending || (!!current && detail.isPending));
  const view: ProposeView = startedOver ? { kind: "form" } : proposeView(detail.data);
  const openButton = current ? (
    <Button size="sm" variant="ghost" onClick={() => assistant.openSession(current)}>Open in the assistant</Button>
  ) : null;

  return (
    <Card title="Propose a pipeline from a repo">
      {loading && <p className={styles.muted}>Loading the proposal.</p>}

      {!loading && view.kind === "form" && (
        <form
          className={styles.form}
          onSubmit={(e) => {
            e.preventDefault();
            start.mutate(repo.trim());
          }}
        >
          <label className={styles.label}>
            Repository URL
            <input type="url" value={repo} onChange={(e) => setRepo(e.target.value)} required />
          </label>
          <Button type="submit" size="sm" disabled={start.isPending || repo.trim() === ""}>Propose</Button>
          {refused && <p role="alert" className={styles.error}>{refused}</p>}
        </form>
      )}

      {view.kind === "reading" && (
        <div className={styles.stack}>
          <p>The assistant is reading the repository. Steps: {view.steps}.</p>
          <div className={styles.actions}>
            {openButton}
            <Button size="sm" variant="ghost" onClick={() => void stop()}>Stop</Button>
          </div>
        </div>
      )}

      {view.kind === "waiting" && (
        <div className={styles.stack}>
          <p>The assistant asks to run {view.tool}.</p>
          <div className={styles.actions}>{openButton}</div>
        </div>
      )}

      {view.kind === "ready" && <Ready view={view} denying={denying} setDenying={setDenying} reason={reason} setReason={setReason} onDecide={decide} actionError={actionError} openButton={openButton} />}

      {view.kind === "opened" && (
        <div className={styles.stack}>
          <p>
            <a href={view.url} target="_blank" rel="noreferrer">Pull request #{view.number} opened</a>
          </p>
          <p>Merge the pull request. Then add the repository to the team bundle as a multibranch job.</p>
          <div className={styles.actions}>
            <Button size="sm" variant="ghost" onClick={() => setStartedOver(true)}>Start over</Button>
          </div>
        </div>
      )}

      {(view.kind === "failed" || view.kind === "noProposal" || view.kind === "denied" || view.kind === "expired" || view.kind === "skipped") && (
        <Ended view={view} detail={detail.data} onAskAgain={() => void askAgain()} onStartOver={() => setStartedOver(true)} />
      )}
    </Card>
  );
}

/** Ready shows the proposal in full and the decision controls. */
function Ready({
  view, denying, setDenying, reason, setReason, onDecide, actionError, openButton,
}: {
  view: Extract<ProposeView, { kind: "ready" }>;
  denying: boolean;
  setDenying(v: boolean): void;
  reason: string;
  setReason(v: string): void;
  onDecide(approve: boolean): Promise<void>;
  actionError: string;
  openButton: ReactNode;
}) {
  const args: PrArgs = view.args;
  return (
    <div className={styles.stack}>
      <dl className={styles.facts}>
        <dt>Repository</dt>
        <dd>{args.repo}</dd>
        <dt>Branch</dt>
        <dd>{args.branch}</dd>
        <dt>Title</dt>
        <dd>{args.title}</dd>
      </dl>
      <pre className={styles.body}>{args.body}</pre>
      {args.files.map((f) => (
        <div key={f.path}>
          <h4 className={styles.path}>{f.path}</h4>
          <pre className={styles.file}>{f.content}</pre>
        </div>
      ))}
      <p className={styles.muted}>Expires {age(view.approval.expiresAt)}</p>
      <div className={styles.actions}>
        <Button size="sm" onClick={() => void onDecide(true)}>Approve</Button>
        {!denying && <Button size="sm" variant="ghost" onClick={() => setDenying(true)}>Deny</Button>}
        {denying && (
          <form
            className={styles.reasonForm}
            onSubmit={(e) => {
              e.preventDefault();
              void onDecide(false);
            }}
          >
            <input aria-label="Reason" maxLength={500} value={reason} onChange={(e) => setReason(e.target.value)} />
            <Button type="submit" size="sm">Confirm deny</Button>
          </form>
        )}
        {openButton}
      </div>
      {actionError && <p role="alert" className={styles.error}>{actionError}</p>}
    </div>
  );
}

/** Ended shows the closing state of a proposal and its next actions. */
function Ended({
  view, detail, onAskAgain, onStartOver,
}: {
  view: Extract<ProposeView, { kind: "failed" | "noProposal" | "denied" | "expired" | "skipped" }>;
  detail: AssistantSessionDetail | undefined;
  onAskAgain(): void;
  onStartOver(): void;
}) {
  const text =
    view.kind === "failed" ? view.text
    : view.kind === "noProposal" ? "The assistant opened no pull request."
    : view.kind === "denied" ? "You denied the pull request."
    : view.kind === "expired" ? "The proposal expired before approval."
    : view.reason;
  const canAsk = view.kind === "noProposal" || view.kind === "denied";
  return (
    <div className={styles.stack}>
      <p role={view.kind === "failed" ? "alert" : undefined}>{text}</p>
      <div className={styles.actions}>
        {canAsk && <Button size="sm" onClick={onAskAgain} disabled={!detail}>Ask again</Button>}
        <Button size="sm" variant="ghost" onClick={onStartOver}>Start over</Button>
      </div>
    </div>
  );
}
