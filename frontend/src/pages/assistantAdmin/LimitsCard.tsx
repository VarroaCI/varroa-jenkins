// LimitsCard edits the four budgets, the approval expiry, and the external
// data sharing level. The parent remounts it on each new version of the
// object, so the form always starts from the stored values.
import { useState } from "react";
import { Card } from "../../components/Card";
import { Button } from "../../components/Button";
import type { AssistantConfigSpec, DataSharingLevel } from "../../types/assistant";
import {
  applyLimits,
  confirmationFor,
  limitsDraftOf,
  validateLimits,
  type LimitsDraft,
} from "./assistantConfigEdit";
import styles from "./LimitsCard.module.css";

interface LimitsCardProps {
  spec: AssistantConfigSpec;
  busy: boolean;
  commit: (spec: AssistantConfigSpec) => Promise<string>;
}

const LEVELS: DataSharingLevel[] = ["none", "metadata", "logs"];

const NUMERIC: { field: "session" | "userDay" | "tenantDay" | "steps"; label: string; id: string }[] = [
  { field: "session", label: "Session budget", id: "assistant-limit-session" },
  { field: "userDay", label: "User day budget", id: "assistant-limit-user-day" },
  { field: "tenantDay", label: "Tenant day budget", id: "assistant-limit-tenant-day" },
  { field: "steps", label: "Steps per session", id: "assistant-limit-steps" },
];

export function LimitsCard({ spec, busy, commit }: LimitsCardProps) {
  const [draft, setDraft] = useState<LimitsDraft>(() => limitsDraftOf(spec));
  const [attempted, setAttempted] = useState(false);
  const [confirm, setConfirm] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const errors = validateLimits(draft);
  const fieldError = (f: keyof LimitsDraft) => (attempted ? errors[f] : undefined);
  const edit = (field: keyof LimitsDraft) => (value: string) => {
    setConfirm([]);
    setDraft((d) => ({ ...d, [field]: value }));
  };

  async function submit() {
    setAttempted(true);
    setError("");
    if (Object.keys(errors).length > 0) return;
    const next = applyLimits(spec, draft);
    const messages = confirmationFor(spec, next);
    if (messages.length > 0 && confirm.length === 0) {
      setConfirm(messages);
      return;
    }
    setSaving(true);
    const message = await commit(next);
    setSaving(false);
    if (message !== "") {
      setError(message);
      setConfirm([]);
    }
  }

  const disabled = busy || saving;

  return (
    <Card title="Limits">
      <form
        aria-label="Limits"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <p className={styles.note}>A running session keeps the budgets it started with. The next start, message, or resume uses the saved values.</p>
        <div className={styles.grid}>
          {NUMERIC.map((n) => (
            <div key={n.field} className={styles.group}>
              <label htmlFor={n.id} className={styles.label}>{n.label}</label>
              <input
                id={n.id}
                className={styles.input}
                inputMode="numeric"
                value={draft[n.field]}
                onChange={(e) => edit(n.field)(e.target.value)}
              />
              {fieldError(n.field) && <p className={styles.fieldError}>{fieldError(n.field)}</p>}
            </div>
          ))}
          <div className={styles.group}>
            <label htmlFor="assistant-limit-expiry" className={styles.label}>Approval expiry</label>
            <input id="assistant-limit-expiry" className={styles.input} value={draft.approvalExpiry} onChange={(e) => edit("approvalExpiry")(e.target.value)} />
            {fieldError("approvalExpiry") && <p className={styles.fieldError}>{fieldError("approvalExpiry")}</p>}
          </div>
          <div className={styles.group}>
            <label htmlFor="assistant-limit-sharing" className={styles.label}>External data sharing</label>
            <select id="assistant-limit-sharing" className={styles.select} value={draft.externalDataSharing} onChange={(e) => edit("externalDataSharing")(e.target.value)}>
              {LEVELS.map((l) => <option key={l} value={l}>{l}</option>)}
            </select>
            {fieldError("externalDataSharing") && <p className={styles.fieldError}>{fieldError("externalDataSharing")}</p>}
          </div>
        </div>

        {confirm.length > 0 && (
          <div role="alertdialog" aria-label="Confirm the limit change" className={styles.confirm}>
            {confirm.map((m) => <p key={m}>{m}</p>)}
            <div className={styles.actions}>
              <Button variant="primary" type="submit" disabled={disabled}>Confirm save</Button>
              <Button type="button" variant="ghost" disabled={disabled} onClick={() => setConfirm([])}>Back</Button>
            </div>
          </div>
        )}

        {confirm.length === 0 && (
          <div className={styles.actions}>
            <Button variant="primary" type="submit" disabled={disabled}>Save limits</Button>
          </div>
        )}
        {error && <p role="alert" className={styles.error}>{error}</p>}
      </form>
    </Card>
  );
}
