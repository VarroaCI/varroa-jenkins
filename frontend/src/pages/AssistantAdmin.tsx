// AssistantAdmin is the Administration page of the assistant: the kill
// switch, the pending approvals, the providers and the defaults of the
// AssistantConfig object, the tenant data levels, and the limits.
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
import { KVGrid } from "../components/KVGrid";
import { age } from "../components/activityTimeline.util";
import styles from "./AssistantAdmin.module.css";
import type {
  AssistantConfigObject,
  AssistantSettings,
  AssistantSettingsUpdate,
  DataSharingLevel,
} from "../types/assistant";

const LEVELS: DataSharingLevel[] = ["none", "metadata", "logs"];

/** CONFIG_CHANGED tells an admin that another save won the version fence. */
export const CONFIG_CHANGED = "The configuration changed while this page was open. The page shows the latest values. Make the change again.";

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
  const [confirming, setConfirming] = useState(false);
  const [defaultProvider, setDefaultProvider] = useState<string | null>(null);
  const [defaultModel, setDefaultModel] = useState<string | null>(null);
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
    mutationFn: (body: AssistantConfigObject) => putAssistantConfig(body),
    onSuccess: (data: AssistantConfigObject) => {
      setError("");
      setDefaultProvider(null);
      setDefaultModel(null);
      queryClient.setQueryData(["assistant-config"], data);
    },
    onError: (err) => {
      if (err instanceof ApiError && err.status === 409) {
        setError(CONFIG_CHANGED);
        setDefaultProvider(null);
        setDefaultModel(null);
        void queryClient.invalidateQueries({ queryKey: ["assistant-config"] });
        return;
      }
      setError(err instanceof ApiError ? err.serverMessage || "The change was not saved." : "The change was not saved.");
    },
  });

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
  const cfg = config.data;
  const providerList = cfg?.spec.providers ?? [];
  const tenants = Object.entries(s?.tenantDataSharing ?? {});
  const shownProvider = defaultProvider ?? cfg?.spec.defaultProvider ?? "";
  const shownModel = defaultModel ?? cfg?.spec.defaultModel ?? "";
  const listed = providerList.some((p) => p.name === shownProvider);
  const modelsOf = providerList.find((p) => p.name === shownProvider)?.models ?? [];
  const defaultsSaveable = cfg !== undefined && (providerList.length === 0 || listed);

  function saveDefaults() {
    if (!cfg) return;
    save.mutate({
      apiVersion: cfg.apiVersion,
      kind: cfg.kind,
      metadata: cfg.metadata,
      spec: { ...cfg.spec, defaultProvider: shownProvider, defaultModel: shownModel },
    });
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
          <Card title="Providers">
            <div className={styles.tableScroll}>
              <table className={styles.table}>
                <thead>
                  <tr><th>Name</th><th>Kind</th><th>Models</th><th>Trust</th><th>Auth</th><th>Base URL</th><th>CIDRs</th><th>Port</th></tr>
                </thead>
                <tbody>
                  {providerList.map((p) => (
                    <tr key={p.name}>
                      <td className={styles.nameCell}>{p.name}</td>
                      <td>{p.kind}</td>
                      <td>{p.models.join(", ")}</td>
                      <td>{p.trust}</td>
                      <td>{p.auth ?? "apiKey"}</td>
                      <td className={styles.mono}>{p.baseURL ?? ""}</td>
                      <td className={styles.mono}>{(p.cidrs ?? []).join(", ")}</td>
                      <td>{p.port ?? ""}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className={styles.note}>Edit the AssistantConfig object varroa-assistant to change this list.</p>
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
              <Button variant="primary" disabled={!defaultsSaveable || save.isPending} onClick={saveDefaults}>
                Save defaults
              </Button>
            </div>
            {providerList.length > 0 && !listed && (
              <p role="status" className={styles.note}>Pick a listed provider as the default.</p>
            )}
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
          <Card title="Limits">
            <KVGrid
              items={[
                { key: "Session budget", value: `${cfg.spec.budgets.session} tokens` },
                { key: "User day budget", value: `${cfg.spec.budgets.userDay} tokens` },
                { key: "Tenant day budget", value: `${cfg.spec.budgets.tenantDay} tokens` },
                { key: "Steps per session", value: String(cfg.spec.budgets.steps) },
                { key: "External data sharing", value: cfg.spec.externalDataSharing },
                { key: "Approval expiry", value: cfg.spec.approvalExpiry },
              ]}
            />
          </Card>
        )}
      </div>
    </SectionPage>
  );
}
