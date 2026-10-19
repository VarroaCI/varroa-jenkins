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

  return (
    <div>
      {explained && (
        <p role="note">Turn on the assistant to explain a failed build.</p>
      )}
      <PausedBanner />

      <Card title="Assistant">
        <label>
          <input
            type="checkbox"
            role="switch"
            aria-label="Assistant on"
            checked={current.enabled}
            onChange={(e) => {
              // The server refuses a provider that the chart no longer lists, so the switch saves Default in that case.
              const base = gone ? { ...current, provider: "", model: "" } : current;
              const next = { ...base, enabled: e.target.checked };
              setModel(next);
              save.mutate(next);
            }}
          />{" "}
          {current.enabled ? "On" : "Off"}
        </label>
      </Card>

      <Card title="Model">
        <label>
          Provider{" "}
          <select
            aria-label="Provider"
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
        </label>{" "}
        <label>
          Model{" "}
          <select aria-label="Model" value={current.model} disabled={gone || listFailed} onChange={(e) => setModel({ ...current, model: e.target.value })}>
            <option value="">Default</option>
            {listFailed && current.model !== "" && <option value={current.model} disabled>{current.model}</option>}
            {(selectedProvider?.models ?? []).map((m) => (
              <option key={m} value={m}>{m}</option>
            ))}
          </select>
        </label>{" "}
        <label>
          Data level{" "}
          <select
            aria-label="Data level"
            value={current.dataSharing}
            onChange={(e) => setModel({ ...current, dataSharing: e.target.value as DataSharingLevel })}
          >
            {LEVELS.map((l) => <option key={l} value={l}>{l}</option>)}
          </select>
        </label>
        {gone && <p role="status">The saved provider {current.provider} is not available. Pick a provider or Default.</p>}
        {listFailed && <p role="alert">{PROVIDERS_FAILED}</p>}
        <div>
          <Button size="sm" disabled={gone} onClick={() => save.mutate(current)}>Save</Button>
        </div>
        {saveError && <p role="alert">{saveError}</p>}
      </Card>

      <Card title="Data">
        <p>Each provider receives at most {current.dataSharing}.</p>
        <p>A tenant level can lower it further.</p>
      </Card>

      <Card title="Session key">
        <p role="status">{keyStatus(me.data)}</p>
        <div>
          <select aria-label="Key provider" value={keyProviderShown} onChange={(e) => setKeyProvider(e.target.value)}>
            {providerList.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
          </select>{" "}
          <input
            type="password"
            autoComplete="off"
            aria-label="API key"
            value={keyValue}
            onChange={(e) => setKeyValue(e.target.value)}
          />{" "}
          <Button size="sm" disabled={keyBusy || !keyValue || !keyProviderShown} onClick={() => void setKey()}>Set key</Button>{" "}
          <Button size="sm" variant="ghost" disabled={keyBusy || !me.data?.sessionKey} onClick={() => void clearKey()}>Clear key</Button>
        </div>
        {keyError && <p role="alert">{keyError}</p>}
      </Card>

      <Card title="Spend today">
        <p>{used} of {budget} tokens used today (UTC)</p>
        <meter min={0} max={budget || 1} value={used} aria-label="Tokens used today" />
      </Card>
    </div>
  );
}
