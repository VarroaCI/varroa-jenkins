// ProvidersCard lists the providers of the AssistantConfig object and holds
// the add, edit, and remove actions. Each save writes the whole object. The
// key of each provider with auth apiKey is set in the key list below the table.
import { useState } from "react";
import { Card } from "../../components/Card";
import { Button } from "../../components/Button";
import type { AssistantConfigProvider, AssistantConfigSpec } from "../../types/assistant";
import { MAX_PROVIDERS, removeProvider } from "./assistantConfigEdit";
import { ProviderForm } from "./ProviderForm";
import { ProviderKeyForm } from "./ProviderKeyForm";
import styles from "./ProvidersCard.module.css";

type Mode = { kind: "add" } | { kind: "edit"; name: string } | null;

interface ProvidersCardProps {
  spec: AssistantConfigSpec;
  /** keySetOf reports a provider's key state, or undefined when the operator has not reported it. */
  keySetOf: (name: string) => boolean | undefined;
  busy: boolean;
  commit: (spec: AssistantConfigSpec) => Promise<string>;
  onKeyChanged: () => void;
}

/** keyLabel words the key state of one provider in the table. */
export function keyLabel(p: AssistantConfigProvider, keySet: boolean | undefined): string {
  if (p.auth === "none") return "not needed";
  if (keySet === undefined) return "not reported";
  return keySet ? "set" : "not set";
}

export function ProvidersCard({ spec, keySetOf, busy, commit, onKeyChanged }: ProvidersCardProps) {
  const [mode, setMode] = useState<Mode>(null);
  const [removing, setRemoving] = useState<string | null>(null);
  const [newDefault, setNewDefault] = useState("");
  const [error, setError] = useState("");

  const others = (name: string) => spec.providers.filter((p) => p.name !== name);
  const removingIsDefault = removing !== null && spec.defaultProvider === removing;
  const needsNewDefault = removingIsDefault && others(removing).length > 0;
  const removingKeyed = removing !== null && (spec.providers.find((p) => p.name === removing)?.auth ?? "apiKey") !== "none";
  const apiKeyProviders = spec.providers.filter((p) => p.auth !== "none");
  const editing = mode?.kind === "edit" ? spec.providers.find((p) => p.name === mode.name) : undefined;

  function startRemove(name: string) {
    setError("");
    setRemoving(name);
    setNewDefault(others(name)[0]?.name ?? "");
  }

  async function confirmRemove() {
    if (removing === null) return;
    const out = removeProvider(spec, removing, needsNewDefault ? newDefault : undefined);
    if (!out.ok) {
      setError(out.message);
      return;
    }
    const message = await commit(out.spec);
    if (message === "") {
      setRemoving(null);
      setError("");
    } else {
      setError(message);
    }
  }

  return (
    <Card title="Providers">
      <div className={styles.tableScroll}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Name</th><th>Kind</th><th>Models</th><th>Trust</th><th>Auth</th><th>Key</th>
              <th>Base URL</th><th>CIDRs</th><th>Port</th><th />
            </tr>
          </thead>
          <tbody>
            {spec.providers.map((p) => (
              <tr key={p.name}>
                <td className={styles.nameCell}>{p.name}{spec.defaultProvider === p.name ? " (default)" : ""}</td>
                <td>{p.kind}</td>
                <td>{p.models.join(", ")}</td>
                <td>{p.trust}</td>
                <td>{p.auth ?? "apiKey"}</td>
                <td>{keyLabel(p, keySetOf(p.name))}</td>
                <td className={styles.mono}>{p.baseURL ?? ""}</td>
                <td className={styles.mono}>{(p.cidrs ?? []).join(", ")}</td>
                <td>{p.port ?? ""}</td>
                <td>
                  <div className={styles.actions}>
                    <Button size="sm" variant="ghost" disabled={busy} onClick={() => { setError(""); setMode({ kind: "edit", name: p.name }); }}>
                      Edit {p.name}
                    </Button>
                    <Button size="sm" variant="ghost" disabled={busy} onClick={() => startRemove(p.name)}>
                      Remove {p.name}
                    </Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {spec.providers.length === 0 && <p className={styles.note}>No provider is listed. Add one to use the assistant.</p>}

      <div className={styles.actions}>
        <Button
          variant="primary"
          disabled={busy || mode !== null || spec.providers.length >= MAX_PROVIDERS}
          onClick={() => { setError(""); setMode({ kind: "add" }); }}
        >
          Add provider
        </Button>
      </div>

      {mode !== null && (
        <ProviderForm
          key={mode.kind === "edit" ? `edit-${mode.name}` : "add"}
          spec={spec}
          initial={editing}
          busy={busy}
          commit={commit}
          onDone={() => setMode(null)}
          onCancel={() => setMode(null)}
        />
      )}

      {removing !== null && (
        <div role="alertdialog" aria-label={`Remove provider ${removing}`} className={styles.dialog}>
          <p>Remove provider {removing}. A saved provider that is removed falls back to the default provider.</p>
          {removingKeyed && <p>Its saved key is removed too.</p>}
          {needsNewDefault && (
            <div className={styles.group}>
              <label htmlFor="assistant-remove-default" className={styles.label}>New default provider</label>
              <select
                id="assistant-remove-default"
                className={styles.select}
                value={newDefault}
                onChange={(e) => setNewDefault(e.target.value)}
              >
                {others(removing).map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
              </select>
            </div>
          )}
          <div className={styles.actions}>
            <Button variant="primary" disabled={busy} onClick={() => void confirmRemove()}>Remove provider</Button>
            <Button variant="ghost" disabled={busy} onClick={() => { setRemoving(null); setError(""); }}>Cancel</Button>
          </div>
          {error && <p role="alert" className={styles.error}>{error}</p>}
        </div>
      )}

      {apiKeyProviders.length > 0 && (
        <>
          <h3 className={styles.keysHeading}>Provider keys</h3>
          <p className={styles.note}>A key is never shown after it is saved. Saving a key replaces the stored key.</p>
          <ul className={styles.keys}>
            {apiKeyProviders.map((p) => (
              <li key={p.name}>
                <span className={styles.keyName}>{p.name}</span>
                <ProviderKeyForm name={p.name} keySet={keySetOf(p.name) === true} onChanged={onKeyChanged} />
              </li>
            ))}
          </ul>
        </>
      )}
    </Card>
  );
}
