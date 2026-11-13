// ProviderKeyForm sets or clears the key of one provider. The key stays in
// the input only until the save ends. It is never read back, never logged,
// and never passed to a query or mutation cache.
import { useState } from "react";
import { deleteAssistantProviderKey, putAssistantProviderKey } from "../../api/client";
import { ApiError } from "../../hooks/useApi";
import { Button } from "../../components/Button";
import styles from "./ProviderKeyForm.module.css";

export const KEY_SET_NOTE = "Key saved. The assistant reads the key within about one minute.";
export const KEY_CLEARED_NOTE = "Key cleared. The assistant stops using it within about one minute.";
const KEY_REFUSED = "The key must be 1 to 4096 bytes and not only white space.";
const KEY_NOT_SAVED = "The key was not saved.";
const KEY_NOT_CLEARED = "The key was not cleared.";

interface ProviderKeyFormProps {
  name: string;
  keySet: boolean;
  onChanged: () => void;
}

/**
 * keyFailure words a refused key save. A key-format refusal has its own text.
 * Any other 400 means the provider changed under this page, so the text asks
 * for a reload. The server text names the rule and never holds the key.
 */
export function keyFailure(err: unknown): string {
  if (!(err instanceof ApiError) || err.status !== 400) return KEY_NOT_SAVED;
  if (err.code === "invalid_key") return KEY_REFUSED;
  return err.serverMessage ? `${err.serverMessage}. Reload the page to see the current providers.` : KEY_NOT_SAVED;
}

export function ProviderKeyForm({ name, keySet, onChanged }: ProviderKeyFormProps) {
  const [value, setValue] = useState("");
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState("");
  const [error, setError] = useState("");

  const setKey = async () => {
    setBusy(true);
    setError("");
    setNote("");
    try {
      await putAssistantProviderKey(name, { apiKey: value });
      setNote(KEY_SET_NOTE);
    } catch (err) {
      setError(keyFailure(err));
    } finally {
      setValue("");
      setBusy(false);
      onChanged();
    }
  };

  const clearKey = async () => {
    setBusy(true);
    setError("");
    setNote("");
    try {
      await deleteAssistantProviderKey(name);
      setNote(KEY_CLEARED_NOTE);
    } catch {
      setError(KEY_NOT_CLEARED);
    } finally {
      setBusy(false);
      onChanged();
    }
  };

  return (
    <div className={styles.keyRow}>
      <div className={styles.field}>
        <input
          type="password"
          autoComplete="new-password"
          aria-label={`API key for ${name}`}
          className={styles.input}
          value={value}
          disabled={busy}
          onChange={(e) => setValue(e.target.value)}
        />
      </div>
      <Button variant="primary" disabled={busy || value.trim() === ""} onClick={() => void setKey()}>
        Set key
      </Button>
      <Button variant="ghost" disabled={busy || !keySet} onClick={() => void clearKey()}>
        Clear key
      </Button>
      {note && <p role="status" className={styles.note}>{note}</p>}
      {error && <p role="alert" className={styles.error}>{error}</p>}
    </div>
  );
}
