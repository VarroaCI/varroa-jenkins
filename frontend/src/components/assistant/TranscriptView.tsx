// TranscriptView renders the rows of a transcript model: the user turns, the
// assistant turns, the tool calls, and the errors. The caller passes the
// approval actions, so the drawer and the Tenant agent page decide differently.
// It keeps the view at the end while the reader stays there, and it shows a
// status line that names what the assistant does now.
import { useEffect, useLayoutEffect, useRef, type ReactNode, type UIEvent } from "react";
import type { AssistantSessionState } from "../../types/assistant";
import { useReducedMotion } from "../../hooks/useReducedMotion";
import { Markdown } from "./Markdown";
import { currentActivity, type PendingMessage } from "./activity";
import { callLabel, cutUtf8, type TranscriptModel } from "./transcript";
import { useRevealedText } from "./reveal";
import styles from "./AssistantDrawer.module.css";

/** PIN_SLACK_PX is the distance from the end that still counts as at the end. */
const PIN_SLACK_PX = 24;

export function TranscriptView({
  model,
  approvalActions,
  sessionState = null,
  pending = null,
}: {
  model: TranscriptModel;
  approvalActions?: (approvalId: string) => ReactNode;
  sessionState?: AssistantSessionState | null;
  pending?: PendingMessage | null;
}) {
  const reduced = useReducedMotion();
  const animate = model.live && !reduced;
  const activity = currentActivity(model, sessionState, pending);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const rowsRef = useRef<HTMLDivElement | null>(null);
  const pinnedRef = useRef(true);

  const onScroll = (e: UIEvent<HTMLDivElement>) => {
    const el = e.currentTarget;
    pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight <= PIN_SLACK_PX;
  };

  // The rows grow as text streams in. While the reader is at the end, keep the end in view.
  useEffect(() => {
    const rows = rowsRef.current;
    if (typeof ResizeObserver === "undefined" || !rows) return;
    const observer = new ResizeObserver(() => {
      const el = scrollRef.current;
      if (el && pinnedRef.current) el.scrollTop = el.scrollHeight;
    });
    observer.observe(rows);
    return () => observer.disconnect();
  }, []);

  // A sent message moves the reader to the end, whatever the scroll position was.
  useLayoutEffect(() => {
    if (!pending) return;
    pinnedRef.current = true;
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [pending]);

  return (
    <div ref={scrollRef} className={styles.transcript} onScroll={onScroll}>
      <div ref={rowsRef} className={styles.rows}>
        {model.rows.map((row) => {
          if (row.kind === "user") return <p key={`u${row.seq}`} className={styles.user}>{row.text}</p>;
          if (row.kind === "turn") {
            const t = model.turns[row.turn];
            if (!t || (t.final && t.text === "")) return null;
            return <AssistantTurn key={`t${row.turn}`} text={t.text} final={t.final} animate={animate} />;
          }
          if (row.kind === "call") {
            const c = model.calls[row.callId];
            if (!c) return null;
            return (
              <details
                key={`c${row.callId}`}
                className={styles.call}
                open={c.approval?.state === "pending" || undefined}
              >
                <summary>{c.name} · {callLabel(c)}</summary>
                <pre>{JSON.stringify(c.arguments, null, 2)}</pre>
                {c.result?.withheld && (
                  <p className={styles.withheld}>Tool result withheld. You do not have permission to view it.</p>
                )}
                {c.result && !c.result.withheld && (
                  <pre className={c.result.isError ? styles.error : undefined}>{cutUtf8(c.result.content, 2048)}</pre>
                )}
                {c.approval?.state === "pending" && approvalActions && (
                  <div className={styles.approval}>{approvalActions(c.approval.id)}</div>
                )}
              </details>
            );
          }
          return <p key={`e${row.seq}`} role="alert" className={styles.error}>{row.code}: {row.message}</p>;
        })}
        {pending && <p className={styles.user}>{pending.text}</p>}
        <div role="status" className={styles.activity}>
          {activity && (
            <>
              <span className={styles.dots} aria-hidden="true"><span /><span /><span /></span>
              <span>{activity.label}</span>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

/** AssistantTurn shows one assistant turn. Live text is revealed word by word; a final or replayed text shows at once. */
function AssistantTurn({ text, final, animate }: { text: string; final: boolean; animate: boolean }) {
  const shown = useRevealedText(text, animate, final);
  return (
    <div className={styles.assistant}>
      <Markdown text={shown} />
      {!final && <span aria-hidden="true">…</span>}
    </div>
  );
}
