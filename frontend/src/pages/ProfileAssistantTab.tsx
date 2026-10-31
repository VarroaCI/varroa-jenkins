// ProfileAssistantTab is the Assistant tab of the profile page: the switch,
// the model and data level, the session key for this login, and the spend
// today. The session key is held only in component state. It never enters
// a query cache, a mutation cache, or browser storage.
import { useState } from "react";
import { useLocation } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  deleteMyAssistantKey,
  getMyAssistant,
  listAssistantProviders,
  putMyAssistant,
  putMyAssistantKey,
} from "../api/client";
import { ApiError } from "../hooks/useApi";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import { useAssistant } from "../components/assistant/AssistantProvider";
import { PausedBanner } from "../components/assistant/PausedBanner";
import { HTTP_LABELS } from "../components/assistant/labels";
import { PROVIDERS_FAILED, savedProviderGone } from "../components/assistant/providerChoice";
import type { AssistantProvider, DataSharingLevel, MyAssistant, MyAssistantUpdate } from "../types/assistant";
import styles from "./ProfileAssistantTab.module.css";

const LEVELS: DataSharingLevel[] = ["none", "metadata", "logs"];

/** keyStatus words the session key line. The key value itself is never shown. */
export function keyStatus(me: MyAssistant | undefined): string {
  if (me?.sessionKey) return `Key set for ${me.sessionKey.provider}, held for this login only`;
  return "No key set";
}

/** keyProviderFor picks the provider of the session key card. The select shows this value and Set key sends it. */
export function keyProviderFor(chosen: string, saved: string, list: AssistantProvider[]): string {
  if (list.some((p) => p.name === chosen)) return chosen;
  if (list.some((p) => p.name === saved)) return saved;
  return list.find((p) => p.default)?.name ?? list[0]?.name ?? "";
}

export function ProfileAssistantTab() {
  const location = useLocation();
  const queryClient = useQueryClient();
  const assistant = useAssistant();
  const me = useQuery({ queryKey: ["me-assistant"], queryFn: getMyAssistant, retry: false });
  const providers = useQuery({
    queryKey: ["assistant-providers"],
    queryFn: listAssistantProviders,
    retry: false,
    enabled: assistant.available,
  });
  const [model, setModel] = useState<MyAssistantUpdate | null>(null);
  const [saveError, setSaveError] = useState("");
  const [keyProvider, setKeyProvider] = useState("");
  const [keyValue, setKeyValue] = useState("");
  const [keyError, setKeyError] = useState("");
  const [keyBusy, setKeyBusy] = useState(false);

  const current: MyAssistantUpdate = model ?? {
    enabled: me.data?.enabled ?? false,
    provider: me.data?.provider ?? "",
    model: me.data?.model ?? "",
    dataSharing: me.data?.dataSharing ?? "none",
  };
  const providerList = providers.data?.items ?? [];
  const keyProviderShown = keyProviderFor(keyProvider, current.provider, providerList);
  const selectedProvider = providerList.find((p) => p.name === current.provider);
  const providerListLoaded = providers.data !== undefined;
  const gone = savedProviderGone(current.provider, providerList, providerListLoaded);
  const listFailed = providers.isError && !providerListLoaded;
  const explained = (location.state as { from?: string } | null)?.from === "explain";

  // Saves all four fields, so a switch and a model change cannot drift apart.
  const save = useMutation({
    mutationFn: (body: MyAssistantUpdate) => putMyAssistant(body),
    onSuccess: (data) => {
      setModel(null);
      setSaveError("");
      queryClient.setQueryData(["me-assistant"], data);
    },
    onError: (err) => {
      setSaveError(err instanceof ApiError ? err.serverMessage || "The settings were not saved." : "The settings were not saved.");
    },
  });

  // The session key calls the API directly. It never goes through useMutation,
  // because a mutation cache would keep the key value.
  const setKey = async () => {
    setKeyBusy(true);
    setKeyError("");
    try {
      await putMyAssistantKey({ provider: keyProviderShown, apiKey: keyValue });
      void queryClient.invalidateQueries({ queryKey: ["me-assistant"] });
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setKeyError("A key needs a browser login. An API key session cannot hold one.");
      } else if (err instanceof ApiError && err.code === "assistant_key_unavailable") {
        setKeyError(HTTP_LABELS.assistant_key_unavailable);
      } else {
        setKeyError("The key was not saved.");
      }
    } finally {
      setKeyValue("");
      setKeyBusy(false);
    }
  };

  const clearKey = async () => {
    setKeyBusy(true);
    setKeyError("");
    try {
      await deleteMyAssistantKey();
      void queryClient.invalidateQueries({ queryKey: ["me-assistant"] });
    } catch {
      setKeyError("The key was not cleared.");
    } finally {
      setKeyBusy(false);
    }
  };

  const used = me.data?.dayTokens ?? 0;
  const budget = me.data?.dayBudget ?? 0;

  const pct = budget > 0 ? Math.min(100, Math.round((used / budget) * 100)) : 0;
  const progressMax = Math.max(1, budget);
  const progressNow = Math.min(progressMax, Math.max(0, used));

  // The form shows no values until the settings arrive. A save before then
  // would write the defaults over the stored settings. A retry after a failed
  // read clears isError while the data is still missing, so the guard tests
  // the data, not the error.
  if (me.data === undefined) {
    return (
      <div className={styles.stack}>
        <Card title="Assistant">
          {me.isError ? (
            <p role="alert">Your assistant settings cannot load right now. Retry when the record store answers.</p>
          ) : (
            <p role="status">Loading assistant settings…</p>
          )}
          {me.isError && (
            <Button variant="ghost" disabled={me.isFetching} onClick={() => void me.refetch()}>Retry</Button>
          )}
        </Card>
      </div>
    );
  }

  return (
    <div className={styles.stack}>
      {explained && (
        <p role="note">Turn on the assistant to explain a failed build.</p>
      )}
      <PausedBanner />

      <Card title="Assistant">
        <label className={styles.switchRow}>
          <input
            type="checkbox"
            role="switch"
            aria-label="Assistant on"
            className={styles.switch}
            checked={current.enabled}
            onChange={(e) => {
              // The server refuses a provider that the chart no longer lists, so the switch saves Default in that case.
              const base = gone ? { ...current, provider: "", model: "" } : current;
              const next = { ...base, enabled: e.target.checked };
              setModel(next);
              save.mutate(next);
            }}
          />
          <span>{current.enabled ? "On" : "Off"}</span>
        </label>
        <p className={styles.note}>The assistant button and its keyboard shortcut appear only when this is on.</p>
      </Card>

      <Card title="Model">
        <div className={styles.formGrid}>
          <div className={styles.formGroup}>
            <label htmlFor="profile-assistant-provider" className={styles.formLabel}>Provider</label>
            <select
              id="profile-assistant-provider"
              aria-label="Provider"
              className={styles.formSelect}
              value={current.provider}
              onChange={(e) => {
                const p = providerList.find((x) => x.name === e.target.value);
                setModel({ ...current, provider: e.target.value, model: p?.models[0] ?? "" });
              }}
            >
              <option value="">Default</option>
              {gone && <option value={current.provider} disabled>{`${current.provider} (removed)`}</option>}
              {listFailed && current.provider !== "" && <option value={current.provider} disabled>{current.provider}</option>}
              {providerList.map((p) => (
                <option key={p.name} value={p.name}>{p.available ? p.name : `${p.name} (unavailable)`}</option>
              ))}
            </select>
          </div>
          <div className={styles.formGroup}>
            <label htmlFor="profile-assistant-model" className={styles.formLabel}>Model</label>
            <select
              id="profile-assistant-model"
              aria-label="Model"
              className={styles.formSelect}
              value={current.model}
              disabled={gone || listFailed}
              onChange={(e) => setModel({ ...current, model: e.target.value })}
            >
              <option value="">Default</option>
              {listFailed && current.model !== "" && <option value={current.model} disabled>{current.model}</option>}
              {(selectedProvider?.models ?? []).map((m) => (
                <option key={m} value={m}>{m}</option>
              ))}
            </select>
          </div>
          <div className={styles.formGroup}>
            <label htmlFor="profile-assistant-level" className={styles.formLabel}>Data level</label>
            <select
              id="profile-assistant-level"
              aria-label="Data level"
              className={styles.formSelect}
              value={current.dataSharing}
              onChange={(e) => setModel({ ...current, dataSharing: e.target.value as DataSharingLevel })}
            >
              {LEVELS.map((l) => <option key={l} value={l}>{l}</option>)}
            </select>
          </div>
        </div>
        {gone && <p role="status" className={styles.note}>The saved provider {current.provider} is not available. Pick a provider or Default.</p>}
        {listFailed && <p role="alert" className={styles.note}>{PROVIDERS_FAILED}</p>}
        <div className={styles.actions}>
          <Button variant="primary" disabled={gone} onClick={() => save.mutate(current)}>Save</Button>
        </div>
        {saveError && <p role="alert" className={styles.error}>{saveError}</p>}
      </Card>

      <Card title="Data">
        <p>Each provider receives at most {current.dataSharing}.</p>
        <p>A tenant level can lower it further.</p>
      </Card>

      <Card title="Session key">
        <p role="status">{keyStatus(me.data)}</p>
        <div className={styles.keyRow}>
          <div className={styles.formGroup}>
            <select
              aria-label="Key provider"
              className={styles.formSelect}
              value={keyProviderShown}
              onChange={(e) => setKeyProvider(e.target.value)}
            >
              {providerList.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
            </select>
          </div>
          <div className={styles.formGroup}>
            <input
              type="password"
              autoComplete="off"
              aria-label="API key"
              className={styles.formInput}
              value={keyValue}
              onChange={(e) => setKeyValue(e.target.value)}
            />
          </div>
          <Button variant="primary" disabled={keyBusy || !keyValue || !keyProviderShown} onClick={() => void setKey()}>Set key</Button>
          <Button variant="ghost" disabled={keyBusy || !me.data?.sessionKey} onClick={() => void clearKey()}>Clear key</Button>
        </div>
        {keyError && <p role="alert" className={styles.error}>{keyError}</p>}
      </Card>

      <Card title="Spend today">
        <p>{used} of {budget} tokens used today (UTC)</p>
        <div
          role="progressbar"
          aria-label="Tokens used today"
          aria-valuemin={0}
          aria-valuemax={progressMax}
          aria-valuenow={progressNow}
          aria-valuetext={`${used} of ${budget} tokens used`}
          className={styles.bar}
        >
          <div className={styles.barFill} style={{ width: `${pct}%` }} />
        </div>
        <p className={styles.note}>{pct}% of the day budget</p>
      </Card>
    </div>
  );
}
