// AssistantAdmin is the Administration page of the assistant: the kill
// switch, the pending approvals, the providers and the defaults of the
// AssistantConfig object, the tenant data levels, and the limits. Every save
// of the object sends the whole object at the version of the read.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  getAssistantConfig,
  getAssistantSettings,
  listAssistantApprovals,
  putAssistantConfig,
  putAssistantSettings,
} from "../api/client";
import { ApiError, isRecordStoreUnreachable } from "../hooks/useApi";
import { SectionPage } from "../components/SectionPage";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import { age } from "../components/activityTimeline.util";
import { configBody } from "./assistantAdmin/assistantConfigEdit";
import { ProvidersCard } from "./assistantAdmin/ProvidersCard";
import { LimitsCard } from "./assistantAdmin/LimitsCard";
import styles from "./AssistantAdmin.module.css";
import type {
  AssistantConfigObject,
  AssistantConfigSpec,
  AssistantSettings,
  AssistantSettingsUpdate,
  DataSharingLevel,
} from "../types/assistant";

const LEVELS: DataSharingLevel[] = ["none", "metadata", "logs"];

/** CONFIG_CHANGED tells an admin that another save won the version fence. */
export const CONFIG_CHANGED = "The configuration changed while this page was open. The page shows the latest values. Make the change again.";
const SAVE_FAILED = "The change was not saved.";

/** pendingCountText words the pending count. A list with a next page reads as "or more". */
export function pendingCountText(count: number, more: boolean): string {
  return more ? `${count} or more` : String(count);
}

export function AssistantAdmin() {
  const queryClient = useQueryClient();
  const settings = useQuery({
    queryKey: ["assistant-settings"],
    queryFn: getAssistantSettings,
    refetchInterval: 30_000,
    retry: false,
  });
  const pending = useQuery({
    queryKey: ["assistant-approvals", "admin-pending"],
    queryFn: () => listAssistantApprovals({ scope: "all", state: "pending" }),
    retry: false,
  });
  const config = useQuery({ queryKey: ["assistant-config"], queryFn: getAssistantConfig, retry: false });
  const cfg = config.data;
  const [confirming, setConfirming] = useState(false);
  const [defaultProvider, setDefaultProvider] = useState<string | null>(null);
  const [defaultModel, setDefaultModel] = useState<string | null>(null);
  const [defaultsError, setDefaultsError] = useState("");
  const [tenant, setTenant] = useState("");
  const [level, setLevel] = useState<DataSharingLevel>("none");
  const [error, setError] = useState("");

  const update = useMutation({
    mutationFn: (body: AssistantSettingsUpdate) => putAssistantSettings(body),
    onSuccess: (data: AssistantSettings) => {
      setError("");
      setConfirming(false);
      queryClient.setQueryData(["assistant-settings"], data);
    },
    onError: (err) => {
      setError(err instanceof ApiError ? err.serverMessage || "The change was not saved." : "The change was not saved.");
    },
  });

  const save = useMutation({
    mutationFn: ({ spec, base }: { spec: AssistantConfigSpec; base: AssistantConfigObject }) =>
      putAssistantConfig(configBody(base, spec)),
    onSuccess: (data: AssistantConfigObject) => {
      queryClient.setQueryData(["assistant-config"], data);
    },
  });

  // commit saves the whole object at the version of the read. It resolves to
  // "" on success, or to the message the form shows. A 409 refetches the object.
  const commit = async (spec: AssistantConfigSpec): Promise<string> => {
    if (!cfg) return "The configuration has not loaded. Reload the page.";
    try {
      await save.mutateAsync({ spec, base: cfg });
      return "";
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        void queryClient.invalidateQueries({ queryKey: ["assistant-config"] });
        return CONFIG_CHANGED;
      }
      return err instanceof ApiError && err.serverMessage ? err.serverMessage : SAVE_FAILED;
    }
  };

  if (isRecordStoreUnreachable(settings.error)) {
    return (
      <SectionPage title="Assistant">
        <p role="alert">Varroa cannot reach the record store. The assistant settings return when the database answers.</p>
        <Button onClick={() => void settings.refetch()}>Retry</Button>
      </SectionPage>
    );
  }

  if (settings.error instanceof ApiError && settings.error.status === 503) {
    return (
      <SectionPage title="Assistant">
        <p>The assistant is off on this install. The chart value assistant.enabled turns it on.</p>
      </SectionPage>
    );
  }

  const s = settings.data;
  const providerList = cfg?.spec.providers ?? [];
  const keySetOf = (name: string): boolean | undefined =>
    cfg?.status?.providers?.find((st) => st.name === name)?.keySet;
  const onKeyChanged = () => {
    void queryClient.invalidateQueries({ queryKey: ["assistant-config"] });
  };
  const egress = cfg?.status?.conditions?.find((c) => c.type === "EgressReady");
  const egressSummary = egress === undefined
    ? "The egress rule has not been reported yet."
    : egress.status === "True"
      ? (egress.message ?? "Every provider has an egress rule.")
      : `Some providers have no egress rule. ${egress.message ?? ""}`.trim();
  const tenants = Object.entries(s?.tenantDataSharing ?? {});
  const shownProvider = defaultProvider ?? cfg?.spec.defaultProvider ?? "";
  const shownModel = defaultModel ?? cfg?.spec.defaultModel ?? "";
  const listed = providerList.some((p) => p.name === shownProvider);
  const modelsOf = providerList.find((p) => p.name === shownProvider)?.models ?? [];
  const defaultsSaveable = cfg !== undefined && (providerList.length === 0 || listed);

  async function saveDefaults() {
    if (!cfg) return;
    const message = await commit({ ...cfg.spec, defaultProvider: shownProvider, defaultModel: shownModel });
    if (message === "") {
      setDefaultsError("");
      setDefaultProvider(null);
      setDefaultModel(null);
    } else {
      setDefaultsError(message);
      if (message === CONFIG_CHANGED) {
        setDefaultProvider(null);
        setDefaultModel(null);
      }
    }
  }

  return (
    <SectionPage title="Assistant" loading={settings.isLoading} empty={!settings.isLoading && !s}>
      <div className={styles.stack}>
        {error && <p role="alert" className={styles.error}>{error}</p>}

        {s && (
          <Card title="Kill switch">
            <p>{s.paused ? `Paused by ${s.pausedBy ?? "someone"}${s.pausedAt ? ` ${age(s.pausedAt)}` : ""}` : "The assistant is running."}</p>
            {s.paused ? (
              <div className={styles.actions}>
                <Button variant="primary" onClick={() => update.mutate({ paused: false })}>Resume</Button>
              </div>
            ) : confirming ? (
              <div role="alertdialog" aria-label="Confirm pause">
                <p>Pausing stops every running conversation and blocks new ones for everyone.</p>
                <div className={styles.actions}>
                  <Button variant="primary" onClick={() => update.mutate({ paused: true })}>Confirm pause</Button>
                  <Button variant="ghost" onClick={() => setConfirming(false)}>Cancel</Button>
                </div>
              </div>
            ) : (
              <div className={styles.actions}>
                <Button onClick={() => setConfirming(true)}>Pause</Button>
              </div>
            )}
          </Card>
        )}

        <Card title="Pending approvals">
          <p>{pendingCountText(pending.data?.items.length ?? 0, !!pending.data?.nextCursor)} pending</p>
        </Card>

        {config.isError && (
          <Card title="Configuration">
            <p role="alert">The assistant configuration did not load. The providers, defaults, and limits return when it answers.</p>
            <div className={styles.actions}>
              <Button onClick={() => void config.refetch()}>Retry</Button>
            </div>
          </Card>
        )}

        {cfg && (
          <ProvidersCard
            spec={cfg.spec}
            keySetOf={keySetOf}
            busy={save.isPending}
            commit={commit}
            onKeyChanged={onKeyChanged}
          />
        )}

        {cfg && (
          <Card title="Egress">
            <p className={styles.note}>{egressSummary}</p>
            <ul className={styles.note}>
              {providerList.map((p) => (
                <li key={p.name} className={styles.mono}>
                  {`${p.name}: ${(p.cidrs ?? []).join(", ") || "no CIDRs"}, port ${p.port ?? "from the base URL"}`}
                </li>
              ))}
            </ul>
          </Card>
        )}

        {cfg && (
          <Card title="Defaults">
            <div className={styles.formGrid}>
              <div className={styles.formGroup}>
                <label htmlFor="assistant-default-provider" className={styles.formLabel}>Provider</label>
                <select
                  id="assistant-default-provider"
                  aria-label="Default provider"
                  className={styles.formSelect}
                  value={shownProvider}
                  onChange={(e) => {
                    setDefaultProvider(e.target.value);
                    setDefaultModel("");
                  }}
                >
                  {!listed && <option value="">No default</option>}
                  {providerList.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
                </select>
              </div>
              <div className={styles.formGroup}>
                <label htmlFor="assistant-default-model" className={styles.formLabel}>Model</label>
                <input
                  id="assistant-default-model"
                  aria-label="Default model"
                  className={styles.formInput}
                  value={shownModel}
                  placeholder={modelsOf[0] ?? ""}
                  disabled={!listed}
                  onChange={(e) => setDefaultModel(e.target.value)}
                />
              </div>
            </div>
            <div className={styles.actions}>
              <Button variant="primary" disabled={!defaultsSaveable || save.isPending} onClick={() => void saveDefaults()}>
                Save defaults
              </Button>
            </div>
            {providerList.length > 0 && !listed && (
              <p role="status" className={styles.note}>Pick a listed provider as the default.</p>
            )}
            {defaultsError && <p role="alert" className={styles.error}>{defaultsError}</p>}
          </Card>
        )}

        {s && (
          <Card title="Tenant data levels">
            <div className={styles.tableScroll}>
              <table className={styles.levelTable}>
                <thead><tr><th>Tenant</th><th>Level</th><th /></tr></thead>
                <tbody>
                  {tenants.map(([t, lv]) => (
                    <tr key={t}>
                      <td className={styles.nameCell}>{t}</td><td>{lv}</td>
                      <td><Button size="sm" variant="ghost" onClick={() => update.mutate({ tenantDataSharing: { remove: t } })}>Remove</Button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className={styles.note}>A tenant with no row reads logs.</p>
            <div className={styles.formGrid}>
              <div className={styles.formGroup}>
                <label htmlFor="assistant-level-tenant" className={styles.formLabel}>Tenant</label>
                <input
                  id="assistant-level-tenant"
                  aria-label="Tenant"
                  className={styles.formInput}
                  value={tenant}
                  onChange={(e) => setTenant(e.target.value)}
                />
              </div>
              <div className={styles.formGroup}>
                <label htmlFor="assistant-level-level" className={styles.formLabel}>Level</label>
                <select
                  id="assistant-level-level"
                  aria-label="Tenant level"
                  className={styles.formSelect}
                  value={level}
                  onChange={(e) => setLevel(e.target.value as DataSharingLevel)}
                >
                  {LEVELS.map((l) => <option key={l} value={l}>{l}</option>)}
                </select>
              </div>
            </div>
            <div className={styles.actions}>
              <Button variant="primary" disabled={!tenant} onClick={() => update.mutate({ tenantDataSharing: { set: { tenant, level } } })}>
                Set level
              </Button>
            </div>
          </Card>
        )}

        {cfg && (
          <LimitsCard
            key={JSON.stringify([cfg.spec.budgets, cfg.spec.approvalExpiry, cfg.spec.externalDataSharing])}
            spec={cfg.spec}
            busy={save.isPending}
            commit={commit}
          />
        )}
      </div>
    </SectionPage>
  );
}
