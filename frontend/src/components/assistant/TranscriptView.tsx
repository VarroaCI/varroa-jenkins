// TranscriptView renders the rows of a transcript model: the user turns, the
// assistant turns, the tool calls, and the errors. The caller passes the
// approval actions, so the drawer and the Team agent page decide differently.
import type { ReactNode } from "react";
import { Markdown } from "./Markdown";
import { callLabel, cutUtf8, type TranscriptModel } from "./transcript";
import styles from "./AssistantDrawer.module.css";

export function TranscriptView({
  model,
  approvalActions,
}: {
  model: TranscriptModel;
  approvalActions?: (approvalId: string) => ReactNode;
}) {
  return (
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
          return <p key={i} role="alert" className={styles.error}>{row.code}: {row.message}</p>;
        })}
      </div>

  );
}
