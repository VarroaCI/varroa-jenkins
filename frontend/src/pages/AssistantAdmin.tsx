// AssistantAdmin is the Administration page of the assistant: the kill
// switch, the pending approvals, the providers, the defaults, the tenant
// data levels, the limits, and the egress.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  getAssistantSettings,
  listAssistantApprovals,
  listAssistantProviders,
  putAssistantSettings,
} from "../api/client";
import { ApiError } from "../hooks/useApi";
import { SectionPage } from "../components/SectionPage";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import { KVGrid } from "../components/KVGrid";
import { age } from "../components/activityTimeline.util";
import styles from "./AssistantAdmin.module.css";
import { PROVIDERS_FAILED, savedProviderGone } from "../components/assistant/providerChoice";
import type { AssistantSettings, AssistantSettingsUpdate, DataSharingLevel } from "../types/assistant";

const LEVELS: DataSharingLevel[] = ["none", "metadata", "logs"];

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
  const providers = useQuery({ queryKey: ["assistant-providers"], queryFn: listAssistantProviders, retry: false });
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

  if (settings.error instanceof ApiError && settings.error.status === 503) {
    return (
      <SectionPage title="Assistant">
        <p>The assistant is off on this install. The chart value assistant.enabled turns it on.</p>
      </SectionPage>
    );
  }

  const s = settings.data;
  const providerList = providers.data?.items ?? [];
  const tenants = Object.entries(s?.tenantDataSharing ?? {});
  const shownProvider = defaultProvider ?? s?.defaultProvider ?? "";
  const shownModel = defaultModel ?? s?.defaultModel ?? "";
  const providerListLoaded = providers.data !== undefined;
  const gone = savedProviderGone(shownProvider, providerList, providerListLoaded);
  const listFailed = providers.isError && !providerListLoaded;

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

        <Card title="Providers">
          <div className={styles.tableScroll}>
            <table className={styles.table}>
              <thead>
                <tr><th>Name</th><th>Kind</th><th>Models</th><th>Trust</th><th>Base URL</th></tr>
              </thead>
              <tbody>
                {providerList.map((p) => (
                  <tr key={p.name}>
                    <td className={styles.nameCell}>{p.name}</td><td>{p.kind}</td><td>{p.models.join(", ")}</td><td>{p.trust}</td><td className={styles.mono}>{p.baseURL}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className={styles.note}>The chart sets this list.</p>
        </Card>

        {s && (
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
                  <option value="">Chart default</option>
                  {gone && <option value={shownProvider} disabled>{`${shownProvider} (removed)`}</option>}
                  {listFailed && shownProvider !== "" && <option value={shownProvider} disabled>{shownProvider}</option>}
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
                  placeholder={providerList.find((p) => p.name === shownProvider)?.models[0] ?? ""}
                  disabled={gone}
                  onChange={(e) => setDefaultModel(e.target.value)}
                />
              </div>
            </div>
            <div className={styles.actions}>
              <Button variant="primary" disabled={gone} onClick={() => update.mutate({ defaultProvider: shownProvider, defaultModel: shownModel })}>
                Save defaults
              </Button>
            </div>
            {gone && <p role="status" className={styles.note}>The saved default provider {shownProvider} is not available. Pick a listed provider or Chart default.</p>}
            {listFailed && <p role="alert" className={styles.note}>{PROVIDERS_FAILED}</p>}
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

        {s && (
          <Card title="Limits">
            <KVGrid
              items={[
                { key: "Session budget", value: `${s.limits.budgets.session} tokens` },
                { key: "User day budget", value: `${s.limits.budgets.userDay} tokens` },
                { key: "Tenant day budget", value: `${s.limits.budgets.tenantDay} tokens` },
                { key: "Steps per session", value: String(s.limits.budgets.steps) },
                { key: "External data sharing", value: s.limits.externalDataSharing },
                { key: "Approval expiry", value: s.limits.approvalExpiry },
              ]}
            />
          </Card>
        )}

        {s && (
          <Card title="Egress">
            {s.limits.egress.networkPolicy ? (
              <>
                <p>Network policy on. CIDRs: {s.limits.egress.cidrs.join(", ")}. Ports: {s.limits.egress.ports.join(", ")}.</p>
                <p className={styles.note}>These are chart values, not a live probe.</p>
              </>
            ) : (
              <p>The BFF egress is not pinned.</p>
            )}
          </Card>
        )}
      </div>
    </SectionPage>
  );
}
