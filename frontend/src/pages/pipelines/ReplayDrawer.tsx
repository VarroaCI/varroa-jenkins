import { useEffect, useRef } from "react";
import { useMutation } from "@tanstack/react-query";
import { EditorState } from "@codemirror/state";
import { EditorView, keymap, lineNumbers } from "@codemirror/view";
import { defaultKeymap } from "@codemirror/commands";
import { replayRun } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import styles from "./ReplayDrawer.module.css";

export interface ReplayDrawerProps {
  runId: string;
  script: string;
  loadedScripts: Record<string, string>;
  onClose: () => void;
  /** Called with the new run's id once the replay request is accepted. */
  onSubmitted: (runId: string) => void;
}

/** errorText names the rule the server gave, when the response carries one. */
function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.serverMessage || err.message;
  return "Varroa could not start this build.";
}

export function ReplayDrawer({ runId, script, loadedScripts, onClose, onSubmitted }: ReplayDrawerProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const viewRef = useRef<EditorView | null>(null);

  // The editor mounts once per drawer. It takes the script as its first
  // text, and the submit reads the current text from the view.
  useEffect(() => {
    if (!containerRef.current) return;
    const state = EditorState.create({
      doc: script,
      extensions: [
        lineNumbers(),
        keymap.of(defaultKeymap),
        EditorView.theme({
          "&": { fontFamily: "var(--mono)", fontSize: "13px", height: "100%" },
          ".cm-scroller": { overflow: "auto" },
          ".cm-content": { caretColor: "var(--text)" },
          ".cm-cursor": { borderLeftColor: "var(--text)" },
          "&.cm-focused .cm-cursor": { borderLeftColor: "var(--accent)" },
          "&.cm-focused": { outline: "none" },
          ".cm-gutters": {
            backgroundColor: "var(--surface-2)",
            borderRight: "1px solid var(--border)",
            color: "var(--text-3)",
          },
          ".cm-activeLineGutter": { backgroundColor: "var(--accent-soft)" },
        }),
      ],
    });
    const view = new EditorView({ state, parent: containerRef.current });
    viewRef.current = view;
    return () => {
      view.destroy();
      viewRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const mutation = useMutation({
    mutationFn: () => replayRun(runId, viewRef.current ? viewRef.current.state.doc.toString() : script),
    onSuccess: (res) => onSubmitted(res.runId),
  });

  const libraries = Object.entries(loadedScripts);

  return (
    <div className={styles.overlay} onClick={onClose}>
      <div
        className={styles.drawer}
        role="dialog"
        aria-modal="true"
        aria-label="Replay this build"
        onClick={(e) => e.stopPropagation()}
      >
        <div className={styles.head}>
          <h2 className={styles.title}>Replay this build</h2>
          <button type="button" className={styles.closeBtn} onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>

        <div className={styles.editor} ref={containerRef} data-testid="replay-editor" />

        {libraries.length > 0 && (
          <div className={styles.libraries}>
            <p className={styles.note}>Library scripts are read only. The replay loads them again from their library.</p>
            {libraries.map(([name, body]) => (
              <details key={name} className={styles.library}>
                <summary className={styles.librarySummary}>{name}</summary>
                <pre className={styles.libraryBody}>{body}</pre>
              </details>
            ))}
          </div>
        )}

        <div className={styles.actions}>
          <button type="button" className={styles.runBtn} disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? "Starting…" : "Replay"}
          </button>
        </div>

        {mutation.isError && <p className={styles.error}>{errorText(mutation.error)}</p>}
      </div>
    </div>
  );
}
