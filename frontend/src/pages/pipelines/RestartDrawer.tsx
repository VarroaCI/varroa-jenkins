import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { restartRun } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import styles from "./RestartDrawer.module.css";

export interface RestartDrawerProps {
  runId: string;
  /** The stages the run can restart from, in run order. */
  stages: string[];
  onClose: () => void;
  /** Called with the new run's id once the restart request is accepted. */
  onSubmitted: (runId: string) => void;
}

/** errorText names the rule the server gave, when the response carries one. */
function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.serverMessage || err.message;
  return "Varroa could not start this build.";
}

/**
 * RestartDrawer lists the restartable stages of a declarative build and
 * starts a new build from the chosen stage. The stage list comes from the
 * restart object of the run, so the drawer never offers a stage the server
 * would refuse.
 */
export function RestartDrawer({ runId, stages, onClose, onSubmitted }: RestartDrawerProps) {
  const [stage, setStage] = useState(stages[0] ?? "");

  const mutation = useMutation({
    mutationFn: () => restartRun(runId, stage),
    onSuccess: (res) => onSubmitted(res.runId),
  });

  return (
    <div className={styles.overlay} onClick={onClose}>
      <div
        className={styles.drawer}
        role="dialog"
        aria-modal="true"
        aria-label="Restart this build from a stage"
        onClick={(e) => e.stopPropagation()}
      >
        <div className={styles.head}>
          <h2 className={styles.title}>Restart from stage</h2>
          <button type="button" className={styles.closeBtn} onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>

        <p className={styles.note}>
          The new build reruns from the stage you choose. Stages before it keep the results of this build.
        </p>

        <fieldset className={styles.stageList}>
          <legend className={styles.legend}>Stage</legend>
          {stages.map((name) => (
            <label key={name} className={styles.stageRow}>
              <input
                type="radio"
                name="restart-stage"
                value={name}
                checked={stage === name}
                onChange={() => setStage(name)}
              />
              <span>{name}</span>
            </label>
          ))}
        </fieldset>

        <div className={styles.actions}>
          <button
            type="button"
            className={styles.runBtn}
            disabled={stage === "" || mutation.isPending}
            onClick={() => mutation.mutate()}
          >
            {mutation.isPending ? "Starting…" : "Restart"}
          </button>
        </div>

        {mutation.isError && <p className={styles.error}>{errorText(mutation.error)}</p>}
      </div>
    </div>
  );
}
