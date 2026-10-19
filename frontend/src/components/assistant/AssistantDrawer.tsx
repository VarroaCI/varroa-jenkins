// AssistantDrawer is the panel that holds one session. It streams the frames
// of the open session, shows the transcript and the approvals, and takes the
// next message. It resumes after the last seq it applied.
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation } from "react-router-dom";
import { useEventStream, type SSEEvent } from "../../hooks/useEventStream";
import { ApiError } from "../../hooks/useApi";
import {
  approveAssistantApproval,
  assistantEventsUrl,
  cancelAssistantSession,
  denyAssistantApproval,
  getAssistantSession,
  listAssistantSessions,
  postAssistantMessage,
} from "../../api/client";
import { useAssistant } from "./AssistantProvider";
import { useNarrowViewport } from "../../hooks/useNarrowViewport";
import { applyEvent, callLabel, cutUtf8, emptyModel, type TranscriptModel } from "./transcript";
import { contextLabel, contextDiffers, routeContext } from "./routeContext";
import { Markdown } from "./Markdown";
import styles from "./AssistantDrawer.module.css";
import type { AssistantEventType, AssistantFrame, AssistantSession, AssistantSessionState } from "../../types/assistant";

const EVENT_NAMES: AssistantEventType[] = [
  "user", "assistant", "tool_call", "tool_result", "approval_requested", "approval_resolved", "error", "done",
];

/** TranscriptStream streams one session and folds its frames into a model. */
function TranscriptStream({
  sessionId,
  onModel,
  onDone,
  resumeFrom,
}: {
  sessionId: string;
  onModel: (m: TranscriptModel) => void;
  onDone: (state: string) => void;
  resumeFrom: number;
}) {
  const modelRef = useRef<TranscriptModel>({ ...emptyModel(), lastSeq: resumeFrom });
  const doneRef = useRef(onDone);
  doneRef.current = onDone;
  const queryClient = useQueryClient();

  const onEvent = (ev: SSEEvent<unknown>) => {
    const seq = Number(ev.id);
    if (!ev.id || Number.isNaN(seq)) return;
    const frame: AssistantFrame = {
      seq,
      type: ev.type as AssistantEventType,
      data: (ev.data ?? {}) as AssistantFrame["data"],
    };
    const next = applyEvent(modelRef.current, frame);
    if (next === modelRef.current) return;
    modelRef.current = next;
    onModel(next);
    if (frame.type === "assistant" || frame.type === "tool_result") {
      void queryClient.invalidateQueries({ queryKey: ["assistant-session", sessionId] });
    }
    if (frame.type === "done") {
      void queryClient.invalidateQueries({ queryKey: ["assistant-session", sessionId] });
      doneRef.current(String(frame.data.state ?? ""));
    }
  };

  useEventStream<unknown>(
    assistantEventsUrl(sessionId, resumeFrom),
    `assistant:${sessionId}`,
    EVENT_NAMES,
    {
      onEvent,
      resumeUrl: () => assistantEventsUrl(sessionId, modelRef.current.lastSeq),
    },
  );
  return null;
}

/** AssistantDrawer renders the open session panel. */
export default function AssistantDrawer() {
  const a = useAssistant();
  const location = useLocation();
  const queryClient = useQueryClient();
  const ctx = routeContext(location.pathname, a.hint);
  const [model, setModel] = useState<TranscriptModel>(emptyModel());
  const [composer, setComposer] = useState("");
  const [refusal, setRefusal] = useState("");
  const [showList, setShowList] = useState(false);
  const [busy, setBusy] = useState(false);
  const closeRef = useRef<HTMLButtonElement | null>(null);
  const narrow = useNarrowViewport();

  useEffect(() => {
    closeRef.current?.focus();
  }, []);

  const sid = a.activeSessionId;
  const detail = useQuery({
    queryKey: ["assistant-session", sid],
    queryFn: () => getAssistantSession(sid as string),
    enabled: !!sid,
    retry: false,
  });
  const session: AssistantSession | undefined = detail.data?.session;
  const ctxDiffers = session ? contextDiffers(session.context, ctx) : false;

  const sessions = useQuery({
    queryKey: ["assistant-sessions"],
    queryFn: () => listAssistantSessions({ limit: 20 }),
    enabled: showList,
  });

  // A new session or a reopened one starts from an empty model.
  useEffect(() => {
    setModel(emptyModel());
  }, [sid, a.epoch]);

  const state = model.state ?? session?.state ?? null;
  const running = state === "running" || state === "waiting_approval";
  const completed = state === "completed";
  const canSend = !busy && (!sid || completed);

  const send = async () => {
    const text = composer.trim();
    if (!text) return;
    setRefusal("");
    setBusy(true);
    try {
      if (!sid) {
        const r = await a.startSession({ context: ctx, message: text });
        if (r.ok) setComposer("");
        else setRefusal(r.text);
      } else {
        await postAssistantMessage(sid, { message: text });
        setComposer("");
        void queryClient.invalidateQueries({ queryKey: ["assistant-session", sid] });
        a.openSession(sid);
      }
    } catch (err) {
      setRefusal(err instanceof ApiError && err.status === 409
        ? "This conversation cannot continue. Start a new conversation."
        : "The message was not sent.");
    } finally {
      setBusy(false);
    }
  };

  const decide = async (approvalId: string, approve: boolean) => {
    try {
      if (approve) await approveAssistantApproval(approvalId);
      else await denyAssistantApproval(approvalId, {});
      a.resolved(sid ?? "");
      void queryClient.invalidateQueries({ queryKey: ["assistant-approvals"] });
    } catch {
      void queryClient.invalidateQueries({ queryKey: ["assistant-session", sid] });
    }
  };

  const stop = () => {
    if (sid) void cancelAssistantSession(sid);
  };

  const sectionRef = useRef<HTMLElement | null>(null);

  // Below 640px the drawer is a modal sheet: Tab stays inside it, the page
  // behind does not scroll, and focus returns to the door on close.
  useEffect(() => {
    if (!narrow) return;
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previous;
      a.doorRef.current?.focus();
    };
  }, [narrow, a.doorRef]);

  const onKey = (e: KeyboardEvent) => {
    if (e.key === "Escape") {
      a.closeDrawer();
      return;
    }
    if (narrow && e.key === "Tab" && sectionRef.current) {
      const items = Array.from(
        sectionRef.current.querySelectorAll<HTMLElement>("button, textarea, input, [href]"),
      ).filter((el) => !el.hasAttribute("disabled"));
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    }
  };

  const panel = (
    <section
      ref={sectionRef}
      className={styles.drawer}
      role={narrow ? "dialog" : "complementary"}
      aria-modal={narrow ? true : undefined}
      aria-label="Assistant"
      onKeyDown={onKey}
    >
      <header className={styles.header}>
        <span className={styles.chip} title={contextLabel(ctx)}>{contextLabel(ctx)}</span>
        {sid && ctxDiffers && (
          <button type="button" className={styles.linkBtn} onClick={a.newConversation}>
            New conversation for this page
          </button>
        )}
        <button type="button" className={styles.linkBtn} onClick={() => setShowList((v) => !v)} aria-expanded={showList}>
          Sessions
        </button>
        <button ref={closeRef} type="button" className={styles.linkBtn} onClick={a.closeDrawer}>Close</button>
      </header>

      {a.paused && (
        <p role="status" className={styles.banner}>
          The assistant is paused{a.pausedBy ? ` by ${a.pausedBy}` : ""}.
        </p>
      )}

      {showList && (
        <ul className={styles.list} aria-label="Sessions">
          {(sessions.data?.items ?? []).map((s) => (
            <li key={s.id}>
              <button
                type="button"
                className={styles.linkBtn}
                aria-current={s.id === sid ? "true" : undefined}
                onClick={() => a.openSession(s.id)}
              >
                {s.state} · {s.startedAt}
              </button>
            </li>
          ))}
        </ul>
      )}

      {sid && (
        <TranscriptStream
          key={`${sid}:${a.epoch}`}
          sessionId={sid}
          resumeFrom={0}
          onModel={setModel}
          onDone={(st) => a.onSessionDone(sid, st as AssistantSessionState)}
        />
      )}

      <div className={styles.transcript}>
        {model.rows.map((row, i) => {
          if (row.kind === "user") return <p key={i} className={styles.user}>{row.text}</p>;
          if (row.kind === "turn") {
            const t = model.turns[row.turn];
            return (
              <div key={i} className={styles.assistant}>
                <Markdown text={t?.text ?? ""} />
                {!t?.final && <span aria-hidden="true">…</span>}
              </div>
            );
          }
          if (row.kind === "call") {
            const c = model.calls[row.callId];
            if (!c) return null;
            return (
              <details key={i} className={styles.call}>
                <summary>{c.name} · {callLabel(c)}</summary>
                <pre>{JSON.stringify(c.arguments, null, 2)}</pre>
                {c.result && <pre className={c.result.isError ? styles.error : undefined}>{cutUtf8(c.result.content, 2048)}</pre>}
                {c.approval?.state === "pending" && (
                  <div className={styles.approval}>
                    <button type="button" onClick={() => void decide(c.approval!.id, true)}>Approve</button>
                    <button type="button" onClick={() => void decide(c.approval!.id, false)}>Deny</button>
                  </div>
                )}
              </details>
            );
          }
          return <p key={i} role="alert" className={styles.error}>{row.code}: {row.message}</p>;
        })}
      </div>

      {refusal && <p role="alert" className={styles.error}>{refusal}</p>}

      <form
        className={styles.composer}
        onSubmit={(e) => {
          e.preventDefault();
          void send();
        }}
      >
        <textarea
          aria-label="Message"
          maxLength={8000}
          value={composer}
          onChange={(e) => setComposer(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <button type="submit" disabled={!canSend || !composer.trim()}>Send</button>
      </form>

      <footer className={styles.footer}>
        <span aria-live="polite">{state ? `State: ${state}` : "No session"}</span>
        {running && <button type="button" onClick={stop}>Stop</button>}
      </footer>
    </section>
  );

  if (!narrow) return panel;
  return (
    <>
      <div className={styles.scrim} role="presentation" onClick={a.closeDrawer} />
      {panel}
    </>
  );
}
