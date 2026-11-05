// AssistantDrawer is the panel that holds one session. It streams the frames
// of the open session, shows the transcript and the approvals, and takes the
// next message. It resumes after the last seq it applied.
import { useEffect, useLayoutEffect, useRef, useState, type CSSProperties, type KeyboardEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation } from "react-router-dom";
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
import { DrawerResizeHandle } from "./DrawerResizeHandle";
import {
  DOCKED_QUERY,
  clampDrawerWidth,
  defaultDrawerWidth,
  maxDrawerWidth,
  readDrawerWidth,
  writeDrawerWidth,
} from "./drawerWidth";
import { contextLabel, contextDiffers, routeContext } from "./routeContext";
import { Button } from "../Button";
import { age } from "../activityTimeline.util";
import { TranscriptView } from "./TranscriptView";
import { TranscriptStream } from "./TranscriptStream";
import { labelFor } from "./labels";
import { unechoedPending, type PendingMessage } from "./activity";
import { returnFocus } from "./focusReturn";
import { useFocusFallback } from "./useFocusFallback";
import styles from "./AssistantDrawer.module.css";
import type { AssistantSession, AssistantSessionState } from "../../types/assistant";

// SPENT_TEXT is the refusal text of a plain 409 on a follow-up. While it
// shows, the drawer also shows the "Start a new conversation" button.
const SPENT_TEXT = "This conversation cannot continue. Start a new conversation.";

/** AssistantDrawer renders the open session panel. */
export default function AssistantDrawer() {
  const a = useAssistant();
  const location = useLocation();
  const queryClient = useQueryClient();
  const ctx = routeContext(location.pathname, a.hint);
  const [model, setModel] = useState<TranscriptModel>(emptyModel());
  const [composer, setComposer] = useState("");
  const [refusal, setRefusal] = useState("");
  // refusalCode is the code of a refused create. It picks the way forward under the text.
  const [refusalCode, setRefusalCode] = useState("");
  const [showList, setShowList] = useState(false);
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<PendingMessage | null>(null);
  const closeRef = useRef<HTMLButtonElement | null>(null);
  // Stop, the two new conversation buttons, Approve, and Deny can unmount while they have focus.
  // Focus then moves to the composer, which stays in the drawer in every state.
  const composerRef = useRef<HTMLTextAreaElement | null>(null);
  const keepFocusInComposer = useFocusFallback(composerRef);
  const narrow = useNarrowViewport();
  // useNarrowViewport answers any media query. Only the docked drawer takes a width handle.
  const docked = useNarrowViewport(DOCKED_QUERY);
  const [storedWidth, setStoredWidth] = useState<number | null>(() => readDrawerWidth());
  const [viewport, setViewport] = useState(() => window.innerWidth);
  useEffect(() => {
    const onResize = () => setViewport(window.innerWidth);
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, []);
  const drawerWidth = clampDrawerWidth(storedWidth ?? defaultDrawerWidth(viewport), viewport);
  const drawerStyle = docked && storedWidth !== null
    ? ({ "--drawer-w": `${drawerWidth}px` } as CSSProperties)
    : undefined;

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
  // A refusal belongs to the session that answered it.
  useEffect(() => {
    setModel(emptyModel());
    setRefusal("");
    setRefusalCode("");
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
    // Send turns disabled when the send starts. Browsers differ on where focus
    // goes from a disabled button, and Safari does not focus a clicked button.
    // Focus moves to the composer first.
    composerRef.current?.focus();
    setRefusal("");
    setRefusalCode("");
    setBusy(true);
    setPending({ text, afterSeq: model.lastSeq, sending: true });
    setComposer("");
    const sent = () => setPending((p) => (p ? { ...p, sending: false } : p));
    const fail = (message: string, code = "") => {
      setPending(null);
      setComposer((c) => (c === "" ? raw : c));
      setRefusal(message);
      setRefusalCode(code);
    };
    try {
      if (!sid) {
        const r = await a.startSession({ context: ctx, message: text });
        if (r.ok) {
          sent();
          return;
        }
        fail(r.text, r.code);
        // A busy refusal opens the session list, so the user can open a running session or end one.
        if (r.code === "assistant_busy") setShowList(true);
      } else {
        await postAssistantMessage(sid, { message: text });
        sent();
        void queryClient.invalidateQueries({ queryKey: ["assistant-session", sid] });
      }
    } catch (err) {
      const spent = err instanceof ApiError && err.status === 409;
      if (spent && sid) void queryClient.invalidateQueries({ queryKey: ["assistant-session", sid] });
      fail(spent ? SPENT_TEXT : "The message was not sent.");
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

  // Both new conversation buttons drop the spent session and keep the typed message.
  const startNewConversation = () => {
    setPending(null);
    setRefusal("");
    setRefusalCode("");
    a.newConversation();
  };
  const spent = !!sid && refusal === SPENT_TEXT;

  const stop = () => {
    if (sid) void cancelAssistantSession(sid);
  };

  const sectionRef = useRef<HTMLElement | null>(null);

  // A layout effect reads the opener before the mount effect moves focus to
  // Close. Its cleanup runs before React removes the drawer from the document.
  // A focus target that the close names in returnToRef goes before the opener.
  // Safari does not focus a clicked button, so a page button names itself in
  // openerRef, and that name goes before the focused element.
  const doorRef = a.doorRef;
  const returnToRef = a.returnToRef;
  const openerRef = a.openerRef;
  useLayoutEffect(() => {
    const drawer = sectionRef.current;
    const active = document.activeElement;
    const focused = active instanceof HTMLElement && active !== document.body && !drawer?.contains(active) ? active : null;
    const opener = openerRef.current ?? focused;
    // The mount clears the targets that the opening click named, which keeps the StrictMode double-run clean.
    openerRef.current = null;
    returnToRef.current = null;
    return () => {
      const returnTo = returnToRef.current;
      returnToRef.current = null;
      // A page button that names itself while the drawer is open does not become the opener.
      openerRef.current = null;
      returnFocus(drawer, opener, doorRef.current, returnTo);
    };
  }, [doorRef, returnToRef, openerRef]);

  // Below 640px the drawer is a modal sheet: Tab stays inside it and the page
  // behind does not scroll.
  useEffect(() => {
    if (!narrow) return;
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previous;
    };
  }, [narrow]);

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
      style={drawerStyle}
      role={narrow ? "dialog" : "complementary"}
      aria-modal={narrow ? true : undefined}
      aria-label="Assistant"
      onKeyDown={onKey}
    >
      {/* The handle exists only while docked. When a viewport change removes it with focus, focus moves to Close. */}
      {docked && (
        <DrawerResizeHandle
          width={drawerWidth}
          max={maxDrawerWidth(viewport)}
          onResize={setStoredWidth}
          onCommit={writeDrawerWidth}
          fallbackFocusRef={closeRef}
        />
      )}
      <header className={styles.header}>
        <span className={styles.chip} title={contextLabel(ctx)}>{contextLabel(ctx)}</span>
        {sid && ctxDiffers && (
          <Button ref={keepFocusInComposer} variant="ghost" size="sm" className={styles.headerBtn} onClick={startNewConversation}>
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
          {sessions.data.items.map((s) => {
            const label = contextLabel(s.context);
            const title = s.firstMessage || label;
            return (
              <li key={s.id}>
                <button
                  type="button"
                  className={styles.sessionRow}
                  aria-current={s.id === sid ? "true" : undefined}
                  onClick={() => { setPending(null); a.openSession(s.id); }}
                >
                  <span className={styles.sessionTitle} title={title}>{title}</span>
                  {s.firstMessage && <span className={styles.sessionContext}>{label}</span>}
                  <span className={styles.sessionMeta}>
                    {labelFor("sessionStates", s.state)} · {age(s.startedAt)}{s.id === sid ? " · Current" : ""}
                  </span>
                </button>
              </li>
            );
          })}
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
            <Button ref={keepFocusInComposer} size="sm" variant="primary" onClick={() => void decide(id, true)}>Approve</Button>
            <Button ref={keepFocusInComposer} size="sm" onClick={() => void decide(id, false)}>Deny</Button>
          </>
        )}
      />

      {refusal && <p role="alert" className={styles.error}>{refusal}</p>}
      {refusal && refusalCode === "assistant_off" && (
        <Link ref={keepFocusInComposer} to="/profile?tab=assistant" className={styles.settingsLink}>
          Open assistant settings
        </Link>
      )}
      {spent && (
        <Button ref={keepFocusInComposer} size="sm" className={styles.startOver} onClick={startNewConversation}>
          <MessageSquarePlus size={14} aria-hidden="true" />
          Start a new conversation
        </Button>
      )}

      <form
        className={styles.composer}
        onSubmit={(e) => {
          e.preventDefault();
          void send();
        }}
      >
        <textarea
          ref={composerRef}
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
        <span aria-live="polite">{state ? `State: ${labelFor("sessionStates", state)}` : "No session"}</span>
        {running && <Button ref={keepFocusInComposer} size="sm" onClick={stop}>Stop</Button>}
      </footer>
    </section>
  );

  // One element shape in every layout keeps the section mounted when the
  // viewport crosses 640px, so focus and the stream stay in place.
  return (
    <>
      {narrow && <div className={styles.scrim} role="presentation" onClick={a.closeDrawer} />}
      {panel}
    </>
  );
}
