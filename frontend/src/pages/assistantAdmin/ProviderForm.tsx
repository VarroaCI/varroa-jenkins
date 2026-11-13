// ProviderForm adds a provider or edits one. It checks the draft with the
// CRD rules, shows any change in data sharing for confirmation, and saves the
// whole object through commit.
import { useState } from "react";
import { Button } from "../../components/Button";
import type { AssistantConfigProvider, AssistantConfigSpec } from "../../types/assistant";
import {
  EMPTY_DRAFT,
  confirmationFor,
  draftOf,
  providerOf,
  upsertProvider,
  validateProvider,
  type ProviderDraft,
  type ProviderErrors,
  type ProviderField,
} from "./assistantConfigEdit";
import styles from "./ProviderForm.module.css";

interface ProviderFormProps {
  spec: AssistantConfigSpec;
  /** initial is the provider being edited. It is undefined when a provider is added. */
  initial?: AssistantConfigProvider;
  busy: boolean;
  /** commit saves the whole object and resolves to "" or to the message to show. */
  commit: (spec: AssistantConfigSpec) => Promise<string>;
  onDone: () => void;
  onCancel: () => void;
}

export function ProviderForm({ spec, initial, busy, commit, onDone, onCancel }: ProviderFormProps) {
  const previousName = initial?.name;
  const [draft, setDraft] = useState<ProviderDraft>(() => (initial ? draftOf(initial) : EMPTY_DRAFT));
  const [attempted, setAttempted] = useState(false);
  const [confirm, setConfirm] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  const taken = spec.providers.map((p) => p.name).filter((n) => n !== previousName);
  const errors: ProviderErrors = validateProvider(draft, taken);
  const fieldError = (f: ProviderField) => (attempted ? errors[f] : undefined);
  const edit = (field: ProviderField) => (value: string) => {
    setConfirm([]);
    setDraft((d) => ({ ...d, [field]: value }));
  };

  async function submit() {
    setAttempted(true);
    setError("");
    if (Object.keys(errors).length > 0) return;
    const outcome = upsertProvider(spec, providerOf(draft), previousName);
    if (!outcome.ok) {
      setError(outcome.message);
      return;
    }
    const messages = confirmationFor(spec, outcome.spec);
    if (messages.length > 0 && confirm.length === 0) {
      setConfirm(messages);
      return;
    }
    setSaving(true);
    const message = await commit(outcome.spec);
    setSaving(false);
    if (message === "") {
      onDone();
    } else {
      setError(message);
      setConfirm([]);
    }
  }

  const disabled = busy || saving;

  return (
    <form
      className={styles.form}
      aria-label={initial ? `Edit provider ${initial.name}` : "Add a provider"}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <div className={styles.grid}>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-name" className={styles.label}>Name</label>
          <input id="assistant-provider-name" className={styles.input} value={draft.name} onChange={(e) => edit("name")(e.target.value)} />
          {fieldError("name") && <p className={styles.fieldError}>{fieldError("name")}</p>}
        </div>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-kind" className={styles.label}>Kind</label>
          <select id="assistant-provider-kind" className={styles.select} value={draft.kind} onChange={(e) => edit("kind")(e.target.value)}>
            <option value="anthropic">anthropic</option>
            <option value="openai-compatible">openai-compatible</option>
          </select>
        </div>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-baseurl" className={styles.label}>Base URL</label>
          <input id="assistant-provider-baseurl" className={styles.input} value={draft.baseURL} onChange={(e) => edit("baseURL")(e.target.value)} />
          {fieldError("baseURL") && <p className={styles.fieldError}>{fieldError("baseURL")}</p>}
        </div>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-models" className={styles.label}>Models</label>
          <textarea id="assistant-provider-models" className={styles.textarea} value={draft.models} onChange={(e) => edit("models")(e.target.value)} />
          {fieldError("models") && <p className={styles.fieldError}>{fieldError("models")}</p>}
        </div>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-trust" className={styles.label}>Trust</label>
          <select id="assistant-provider-trust" className={styles.select} value={draft.trust} onChange={(e) => edit("trust")(e.target.value)}>
            <option value="external">external</option>
            <option value="internal">internal</option>
          </select>
        </div>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-auth" className={styles.label}>Auth</label>
          <select id="assistant-provider-auth" className={styles.select} value={draft.auth} onChange={(e) => edit("auth")(e.target.value)}>
            <option value="apiKey">apiKey</option>
            <option value="none">none</option>
          </select>
        </div>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-cidrs" className={styles.label}>CIDRs</label>
          <textarea id="assistant-provider-cidrs" className={styles.textarea} value={draft.cidrs} onChange={(e) => edit("cidrs")(e.target.value)} />
          {fieldError("cidrs") && <p className={styles.fieldError}>{fieldError("cidrs")}</p>}
        </div>
        <div className={styles.group}>
          <label htmlFor="assistant-provider-port" className={styles.label}>Port</label>
          <input id="assistant-provider-port" className={styles.input} inputMode="numeric" value={draft.port} onChange={(e) => edit("port")(e.target.value)} />
          {fieldError("port") && <p className={styles.fieldError}>{fieldError("port")}</p>}
        </div>
      </div>

      {confirm.length > 0 && (
        <div role="alertdialog" aria-label="Confirm the provider change" className={styles.confirm}>
          {confirm.map((m) => <p key={m}>{m}</p>)}
          <div className={styles.actions}>
            <Button variant="primary" type="submit" disabled={disabled}>Confirm save</Button>
            <Button type="button" variant="ghost" disabled={disabled} onClick={() => setConfirm([])}>Back</Button>
          </div>
        </div>
      )}

      {confirm.length === 0 && (
        <div className={styles.actions}>
          <Button variant="primary" type="submit" disabled={disabled}>Save provider</Button>
          <Button type="button" variant="ghost" disabled={disabled} onClick={onCancel}>Cancel</Button>
        </div>
      )}
      {error && <p role="alert" className={styles.error}>{error}</p>}
    </form>
  );
}
