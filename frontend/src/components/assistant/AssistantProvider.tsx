// AssistantProvider holds the assistant state the shell, the drawer, and the
// pages share: availability, the open session, the page hint, and the
// pending run notes. It answers the four availability questions in one
// place, so no door decides them alone.
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useOptionalAuth } from "../../context/AuthContext";
import { canDoGlobal, usePermissions } from "../../hooks/usePermissions";
import {
  createAssistantSession,
  createRunNote,
  getAssistantSession,
  getAssistantSettings,
  getMyAssistant,
} from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import type { AssistantSessionContext, AssistantSessionState } from "../../types/assistant";
import type { RouteHint } from "./routeContext";
import { pendingNotes } from "./pendingNotes";

/** StartResult is what startSession answers. */
export type StartResult = { ok: true; sessionId: string } | { ok: false; code: string; text: string };

export interface AssistantState {
  /** answered is true once the availability question has an answer, even a refusal. */
  answered: boolean;
  available: boolean;
  switchOn: boolean;
  canUse: boolean;
  paused: boolean;
  pausedBy: string | null;
  pausedAt: string | null;
  drawerOpen: boolean;
  activeSessionId: string | null;
  /** epoch changes when a session opens or starts. The drawer then streams that session from seq 0. */
  epoch: number;
  hint: RouteHint;
  setHint(h: RouteHint): void;
  toggleDrawer(): void;
  openSession(id: string): void;
  openWithMessage(text: string, ctx: AssistantSessionContext): void;
  explainRun(runId: string, ctx: AssistantSessionContext): void;
  startSession(body: { context: AssistantSessionContext; message?: string; prompt?: string }): Promise<StartResult>;
  /** resolved refreshes the approval and session reads after a decision. It does not change epoch. */
  resolved(sessionId: string): void;
  onSessionDone(sessionId: string, state: AssistantSessionState): void;
  closeDrawer(): void;
  newConversation(): void;
  registerDoor(el: HTMLElement | null): void;
  doorRef: { current: HTMLElement | null };
}

const OFF: AssistantState = {
  answered: false,
  available: false,
  switchOn: false,
  canUse: false,
  paused: false,
  pausedBy: null,
  pausedAt: null,
  drawerOpen: false,
  activeSessionId: null,
  epoch: 0,
  hint: {},
  setHint: () => {},
  toggleDrawer: () => {},
  openSession: () => {},
  openWithMessage: () => {},
  explainRun: () => {},
  startSession: async () => ({ ok: false, code: "assistant_off", text: "The assistant is off." }),
  resolved: () => {},
  onSessionDone: () => {},
  closeDrawer: () => {},
  newConversation: () => {},
  registerDoor: () => {},
  doorRef: { current: null },
};

const AssistantCtx = createContext<AssistantState>(OFF);

/** useAssistant reads the shared assistant state. Outside a provider it is off. */
export function useAssistant(): AssistantState {
  return useContext(AssistantCtx);
}

const LAST_KEY = (subject: string) => `varroa-assistant-last-session:${subject}`;

function readLast(subject: string): string | null {
  try {
    return window.localStorage.getItem(LAST_KEY(subject));
  } catch {
    return null;
  }
}

function writeLast(subject: string, id: string): void {
  try {
    window.localStorage.setItem(LAST_KEY(subject), id);
  } catch {
    // A blocked store only loses the remembered session.
  }
}

/** useAssistantContextHint sets the page hint while the calling page is mounted. */
export function useAssistantContextHint(hint: RouteHint): void {
  const a = useAssistant();
  const { setHint } = a;
  const key = JSON.stringify(hint);
  useEffect(() => {
    setHint(JSON.parse(key) as RouteHint);
    return () => setHint({});
  }, [key, setHint]);
}

/** AssistantProvider wraps the shell. */
export function AssistantProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const user = useOptionalAuth()?.user;
  const subject = user?.subject ?? "";
  const perms = usePermissions();

  const me = useQuery({
    queryKey: ["me-assistant"],
    queryFn: getMyAssistant,
    refetchOnWindowFocus: true,
    retry: false,
    // A 403 means the caller has no assistant grant. The shell keeps the door off.
    throwOnError: false,
  });
  const available = me.data?.available === true;
  const switchOn = available && me.data?.enabled === true;
  const canUse = available && canDoGlobal(perms.data, "assistant", "use");

  const settings = useQuery({
    queryKey: ["assistant-settings"],
    queryFn: getAssistantSettings,
    enabled: available,
    refetchInterval: 60_000,
    retry: false,
    throwOnError: false,
  });
  const paused = settings.data?.paused ?? false;

  const [drawerOpen, setDrawerOpen] = useState(false);
  const [activeSessionId, setActiveSessionId] = useState<string | null>(null);
  const [epoch, setEpoch] = useState(0);
  const [hint, setHintState] = useState<RouteHint>({});
  const doorRef = useRef<HTMLElement | null>(null);

  const setHint = useCallback((h: RouteHint) => setHintState(h), []);
  const registerDoor = useCallback((el: HTMLElement | null) => {
    doorRef.current = el;
  }, []);

  const pending = useMemo(() => pendingNotes(subject), [subject]);

  const postPendingNote = useCallback(
    (sessionId: string, runId: string) => {
      createRunNote(runId, { sessionId })
        .then(() => {
          pending.remove(sessionId);
          void queryClient.invalidateQueries({ queryKey: ["run-notes", runId] });
        })
        .catch((err: unknown) => {
          if (err instanceof ApiError && err.status === 409) pending.remove(sessionId);
        });
    },
    [pending, queryClient],
  );

  const onSessionDone = useCallback(
    (sessionId: string, state: AssistantSessionState) => {
      const entry = pending.list().find((n) => n.sessionId === sessionId);
      if (!entry) return;
      if (state === "completed") postPendingNote(sessionId, entry.runId);
      else if (["failed", "cancelled", "skipped", "expired"].includes(state)) pending.remove(sessionId);
    },
    [pending, postPendingNote],
  );

  // The pending notes are checked whether or not the drawer is open, so a
  // finished explain session posts its note after the drawer closes.
  useEffect(() => {
    if (!subject) return;
    const check = () => {
      pending.prune();
      for (const entry of pending.list()) {
        getAssistantSession(entry.sessionId)
          .then((d) => onSessionDone(entry.sessionId, d.session.state))
          .catch(() => {
            // A session that cannot be read keeps its pending note.
          });
      }
    };
    check();
    const timer = window.setInterval(check, 15_000);
    return () => window.clearInterval(timer);
  }, [subject, pending, onSessionDone]);

  const openSession = useCallback(
    (id: string) => {
      setActiveSessionId(id);
      if (subject) writeLast(subject, id);
      setDrawerOpen(true);
      setEpoch((e) => e + 1);
    },
    [subject],
  );

  const toggleDrawer = useCallback(() => {
    setDrawerOpen((open) => {
      if (open) return false;
      const last = subject ? readLast(subject) : null;
      if (last && !activeSessionId) setActiveSessionId(last);
      return true;
    });
  }, [subject, activeSessionId]);

  const closeDrawer = useCallback(() => setDrawerOpen(false), []);
  const newConversation = useCallback(() => setActiveSessionId(null), []);

  const startSession = useCallback(
    async (body: { context: AssistantSessionContext; message?: string; prompt?: string }): Promise<StartResult> => {
      try {
        const s = await createAssistantSession(body);
        setActiveSessionId(s.id);
        if (subject) writeLast(subject, s.id);
        setDrawerOpen(true);
        setEpoch((e) => e + 1);
        return { ok: true, sessionId: s.id };
      } catch (err) {
        const code = err instanceof ApiError ? err.code : "";
        if (err instanceof ApiError && err.status === 409 && code === "assistant_off") {
          return { ok: false, code, text: "The assistant is off." };
        }
        if (err instanceof ApiError && err.status === 429) {
          return { ok: false, code: "assistant_busy", text: "The assistant is busy. Try again soon." };
        }
        return { ok: false, code: code || "error", text: err instanceof Error ? err.message : "The request failed." };
      }
    },
    [subject],
  );

  const openWithMessage = useCallback(
    (text: string, ctx: AssistantSessionContext) => {
      void startSession({ context: ctx, message: text });
    },
    [startSession],
  );

  const explainRun = useCallback(
    (runId: string, ctx: AssistantSessionContext) => {
      void startSession({ context: ctx, prompt: "explain-failure" }).then((r) => {
        if (r.ok) pending.add({ sessionId: r.sessionId, runId });
      });
    },
    [startSession, pending],
  );

  // A decision changes no epoch. The open stream folds the approval_resolved
  // frame and the frames of the resumed run, so the transcript stays in place.
  const resolved = useCallback(
    (sessionId: string) => {
      void queryClient.invalidateQueries({ queryKey: ["assistant-approvals"] });
      if (sessionId) void queryClient.invalidateQueries({ queryKey: ["assistant-session", sessionId] });
    },
    [queryClient],
  );

  const value = useMemo<AssistantState>(
    () => ({
      answered: me.isFetched,
      available,
      switchOn,
      canUse,
      paused,
      pausedBy: settings.data?.pausedBy ?? null,
      pausedAt: settings.data?.pausedAt ?? null,
      drawerOpen,
      activeSessionId,
      epoch,
      hint,
      setHint,
      toggleDrawer,
      openSession,
      openWithMessage,
      explainRun,
      startSession,
      resolved,
      onSessionDone,
      closeDrawer,
      newConversation,
      registerDoor,
      doorRef,
    }),
    [
      me.isFetched, available, switchOn, canUse, paused, settings.data, drawerOpen, activeSessionId, epoch, hint,
      setHint, toggleDrawer, openSession, openWithMessage, explainRun, startSession, resolved,
      onSessionDone, closeDrawer, registerDoor,
    ],
  );

  return <AssistantCtx.Provider value={value}>{children}</AssistantCtx.Provider>;
}
