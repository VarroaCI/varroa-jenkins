// NotesCard lists notes. In run mode it sits on a build page, offers the
// Explain action, and takes a note from a person who may write the build.
// In job mode it sits on a job page, lists the newest ten notes, and takes
// no note. A note from an assistant session and a note a person wrote share
// one list.
import { useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Sparkles } from "lucide-react";
import { createRunNote, listJobNotes, listRunNotes } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import { useAssistant } from "../../components/assistant/AssistantProvider";
import { routeContext } from "../../components/assistant/routeContext";
import { Markdown } from "../../components/assistant/Markdown";
import { PausedBanner } from "../../components/assistant/PausedBanner";
import type { RunNote } from "../../types/assistant";
import styles from "./NotesCard.module.css";

/** NOTE_LIMIT is the longest note body the record store keeps, in bytes. */
export const NOTE_LIMIT = 16384;

export type NotesCardProps =
  | { mode: "run"; runId: string; canWrite: boolean; hasDigest: boolean }
  | { mode: "job"; tenant: string; job: string };

export function NotesCard(props: NotesCardProps) {
  if (props.mode === "run") return <RunNotes {...props} />;
  return <JobNotes tenant={props.tenant} job={props.job} />;
}

function JobNotes({ tenant, job }: { tenant: string; job: string }) {
  const notes = useQuery({
    queryKey: ["job-notes", tenant, job],
    queryFn: () => listJobNotes(tenant, job),
    retry: false,
  });
  const items: RunNote[] = notes.data?.items ?? [];
  if (items.length === 0) return null;
  return (
    <Card title="Notes">
      {items.map((n) => (
        <article key={n.id} id={`note-${n.id}`} tabIndex={-1} className={styles.note}>
          <div className={styles.meta}>
            {n.runId ? (
              <Link to={`/pipelines/${encodeURIComponent(tenant)}/${encodeURIComponent(job)}/${n.runId}`}>
                Build #{n.buildNumber ?? "?"}
              </Link>
            ) : (
              <span>Job note</span>
            )}
            <span>{n.principal}</span>
            <time dateTime={n.createdAt}>{new Date(n.createdAt).toLocaleString()}</time>
          </div>
          <Markdown text={n.body} />
        </article>
      ))}
    </Card>
  );
}

function RunNotes({ runId, canWrite, hasDigest }: { runId: string; canWrite: boolean; hasDigest: boolean }) {
  const assistant = useAssistant();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [askAgain, setAskAgain] = useState<string | null>(null);

  const notes = useQuery({
    queryKey: ["run-notes", runId],
    queryFn: () => listRunNotes(runId),
    retry: false,
  });
  const items: RunNote[] = notes.data?.items ?? [];
  const newestAnswer = [...items].reverse().find((n) => n.sessionId);

  const post = useMutation({
    mutationFn: (body: string) => createRunNote(runId, { body }),
    onSuccess: () => {
      setDraft("");
      setError("");
      void queryClient.invalidateQueries({ queryKey: ["run-notes", runId] });
    },
    onError: (err) => {
      if (err instanceof ApiError && err.code === "note_too_long") {
        setError("The note is longer than 16384 bytes. Shorten it.");
      } else {
        setError("The note was not saved.");
      }
    },
  });

  const explain = () => {
    if (!assistant.switchOn) {
      navigate("/profile?tab=assistant", { state: { from: "explain" } });
      return;
    }
    if (newestAnswer) {
      const el = document.getElementById(`note-${newestAnswer.id}`);
      el?.scrollIntoView?.();
      el?.focus();
      setAskAgain(newestAnswer.id);
      return;
    }
    assistant.explainRun(runId, routeContext(location.pathname, assistant.hint));
  };

  const askAgainNow = () => {
    assistant.explainRun(runId, routeContext(location.pathname, assistant.hint));
    setAskAgain(null);
  };

  const bytes = new TextEncoder().encode(draft).length;

  return (
    <Card title="Notes">
      {assistant.paused && <PausedBanner />}
      {hasDigest && assistant.canUse && (
        <div className={styles.explainRow}>
          <Button variant="ghost" size="sm" disabled={assistant.paused} onClick={explain}>
            <Sparkles size={14} aria-hidden /> Explain this failure
          </Button>
          {askAgain && (
            <Button variant="ghost" size="sm" onClick={askAgainNow}>Ask again</Button>
          )}
        </div>
      )}
      {items.length === 0 && <p className={styles.empty}>No notes on this build.</p>}
      {items.map((n) => (
        <article key={n.id} id={`note-${n.id}`} tabIndex={-1} className={styles.note}>
          <div className={styles.meta}>
            {n.sessionId && <Sparkles size={12} aria-label="Assistant answer" />}
            <span>{n.sessionId ? `Assistant answer for ${n.principal}` : n.principal}</span>
            <time dateTime={n.createdAt} title={new Date(n.createdAt).toLocaleString()}>
              {new Date(n.createdAt).toLocaleString()}
            </time>
          </div>
          <Markdown text={n.body} />
        </article>
      ))}
      {canWrite && (
        <form
          className={styles.compose}
          onSubmit={(e) => {
            e.preventDefault();
            if (draft.trim()) post.mutate(draft.trim());
          }}
        >
          <textarea
            aria-label="Write a note"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
          />
          <span className={styles.count}>{bytes} / {NOTE_LIMIT}</span>
          {error && <p role="alert" className={styles.error}>{error}</p>}
          <Button type="submit" size="sm" disabled={!draft.trim() || bytes > NOTE_LIMIT}>Post</Button>
        </form>
      )}
    </Card>
  );
}
