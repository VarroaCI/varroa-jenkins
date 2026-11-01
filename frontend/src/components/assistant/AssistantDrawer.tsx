// AssistantDrawer is the panel that holds one session. It streams the frames
// of the open session, shows the transcript and the approvals, and takes the
// next message. It resumes after the last seq it applied.
import { useEffect, useRef, useState, type KeyboardEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useLocation } from "react-router-dom";
import { History, MessageSquarePlus, X } from "lucide-react";
import { ApiError } from "../../hooks/useApi";
import {
  approveAssistantApproval,
  cancelAssistantSession,
  denyAssistantApproval,
  getAssistantSession,
  listAssistantSessions,
  postAssistantMessage,
} from "../../api/client";
import { useAssistant } from "./AssistantProvider";
import { useNarrowViewport } from "../../hooks/useNarrowViewport";
import { emptyModel, type TranscriptModel } from "./transcript";
import { contextLabel, contextDiffers, routeContext } from "./routeContext";
import { Button } from "../Button";
import { age } from "../activityTimeline.util";
import { TranscriptView } from "./TranscriptView";
import { TranscriptStream } from "./TranscriptStream";
import { unechoedPending, type PendingMessage } from "./activity";
import styles from "./AssistantDrawer.module.css";
import type { AssistantSession, AssistantSessionState } from "../../types/assistant";

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
  const [pending, setPending] = useState<PendingMessage | null>(null);
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
  const shownPending = unechoedPending(model, pending);
  // A send waits for the replay to fold, so afterSeq is the stream's last seq.
  const canSend = !busy && !shownPending && (!sid || (completed && model.live));

  const send = async () => {
    if (!canSend) return;
    const raw = composer;
    const text = raw.trim();
    if (!text) return;
    setRefusal("");
    setBusy(true);
    setPending({ text, afterSeq: model.lastSeq, sending: true });
    setComposer("");
    const sent = () => setPending((p) => (p ? { ...p, sending: false } : p));
    const fail = (message: string) => {
      setPending(null);
      setComposer((c) => (c === "" ? raw : c));
      setRefusal(message);
    };
    try {
      if (!sid) {
        const r = await a.startSession({ context: ctx, message: text });
        if (r.ok) sent();
        else fail(r.text);
      } else {
        await postAssistantMessage(sid, { message: text });
        sent();
        void queryClient.invalidateQueries({ queryKey: ["assistant-session", sid] });
      }
    } catch (err) {
      fail(err instanceof ApiError && err.status === 409
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
          <Button variant="ghost" size="sm" className={styles.headerBtn} onClick={() => { setPending(null); a.newConversation(); }}>
            <MessageSquarePlus size={14} aria-hidden="true" />
            New conversation for this page
          </Button>
        )}
        <Button
          variant="ghost"
          size="sm"
          className={styles.headerBtn}
          onClick={() => setShowList((v) => !v)}
          aria-expanded={showList}
        >
          <History size={14} aria-hidden="true" />
          Sessions
        </Button>
        <Button
          ref={closeRef}
          variant="ghost"
          size="sm"
          className={styles.headerBtn}
          aria-label="Close"
          title="Close"
          onClick={a.closeDrawer}
        >
          <X size={16} aria-hidden="true" />
        </Button>
      </header>

      {a.paused && (
        <p role="status" className={styles.banner}>
          The assistant is paused{a.pausedBy ? ` by ${a.pausedBy}` : ""}.
        </p>
      )}

      {showList && sessions.isLoading && <p className={styles.empty}>Loading sessions…</p>}
      {showList && sessions.isError && <p role="alert" className={styles.empty}>The sessions did not load.</p>}
      {showList && sessions.data && sessions.data.items.length === 0 && (
        <p className={styles.empty}>No sessions yet.</p>
      )}
      {showList && sessions.data && sessions.data.items.length > 0 && (
        <ul className={styles.list} aria-label="Sessions">
          {sessions.data.items.map((s) => (
            <li key={s.id}>
              <button
                type="button"
                className={styles.sessionRow}
                aria-current={s.id === sid ? "true" : undefined}
                onClick={() => { setPending(null); a.openSession(s.id); }}
              >
                <span>{contextLabel(s.context)}</span>
                <span className={styles.sessionMeta}>
                  {s.state} · {age(s.startedAt)}{s.id === sid ? " · Current" : ""}
                </span>
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

      <TranscriptView
        model={model}
        sessionState={session?.state ?? null}
        pending={shownPending}
        approvalActions={(id) => (
          <>
            <Button size="sm" variant="primary" onClick={() => void decide(id, true)}>Approve</Button>
            <Button size="sm" onClick={() => void decide(id, false)}>Deny</Button>
          </>
        )}
      />

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
              if (canSend) void send();
            }
          }}
        />
        <Button type="submit" variant="primary" disabled={!canSend || !composer.trim()}>Send</Button>
      </form>

      <footer className={styles.footer}>
        <span aria-live="polite">{state ? `State: ${state}` : "No session"}</span>
        {running && <Button size="sm" onClick={stop}>Stop</Button>}
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
