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
      {error && <p role="alert">{error}</p>}

      {s && (
        <Card title="Kill switch">
          <p>{s.paused ? `Paused by ${s.pausedBy ?? "someone"} at ${s.pausedAt ?? ""}` : "The assistant is running."}</p>
          {s.paused ? (
            <Button onClick={() => update.mutate({ paused: false })}>Resume</Button>
          ) : confirming ? (
            <div role="alertdialog" aria-label="Confirm pause">
              <p>Pausing stops every running conversation and blocks new ones for everyone.</p>
              <Button onClick={() => update.mutate({ paused: true })}>Confirm pause</Button>{" "}
              <Button variant="ghost" onClick={() => setConfirming(false)}>Cancel</Button>
            </div>
          ) : (
            <Button variant="ghost" onClick={() => setConfirming(true)}>Pause</Button>
          )}
        </Card>
      )}

      <Card title="Pending approvals">
        <p>{pendingCountText(pending.data?.items.length ?? 0, !!pending.data?.nextCursor)} pending</p>
      </Card>

      <Card title="Providers">
        <table>
          <thead>
            <tr><th>Name</th><th>Kind</th><th>Models</th><th>Trust</th><th>Base URL</th></tr>
          </thead>
          <tbody>
            {providerList.map((p) => (
              <tr key={p.name}>
                <td>{p.name}</td><td>{p.kind}</td><td>{p.models.join(", ")}</td><td>{p.trust}</td><td>{p.baseURL}</td>
              </tr>
            ))}
          </tbody>
        </table>
        <p>The chart sets this list.</p>
      </Card>

      {s && (
        <Card title="Defaults">
          <label>
            Provider{" "}
            <select
              aria-label="Default provider"
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
          </label>{" "}
          <label>
            Model{" "}
            <input
              aria-label="Default model"
              value={shownModel}
              placeholder={providerList.find((p) => p.name === shownProvider)?.models[0] ?? ""}
              disabled={gone}
              onChange={(e) => setDefaultModel(e.target.value)}
            />
          </label>{" "}
          <Button size="sm" disabled={gone} onClick={() => update.mutate({ defaultProvider: shownProvider, defaultModel: shownModel })}>
            Save defaults
          </Button>
          {gone && <p role="status">The saved default provider {shownProvider} is not available. Pick a listed provider or Chart default.</p>}
          {listFailed && <p role="alert">{PROVIDERS_FAILED}</p>}
        </Card>
      )}

      {s && (
        <Card title="Tenant data levels">
          <table>
            <thead><tr><th>Tenant</th><th>Level</th><th /></tr></thead>
            <tbody>
              {tenants.map(([t, lv]) => (
                <tr key={t}>
                  <td>{t}</td><td>{lv}</td>
                  <td><Button size="sm" variant="ghost" onClick={() => update.mutate({ tenantDataSharing: { remove: t } })}>Remove</Button></td>
                </tr>
              ))}
            </tbody>
          </table>
          <p>A tenant with no row reads logs.</p>
          <label>
            Tenant{" "}
            <input aria-label="Tenant" value={tenant} onChange={(e) => setTenant(e.target.value)} />
          </label>{" "}
          <label>
            Level{" "}
            <select aria-label="Tenant level" value={level} onChange={(e) => setLevel(e.target.value as DataSharingLevel)}>
              {LEVELS.map((l) => <option key={l} value={l}>{l}</option>)}
            </select>
          </label>{" "}
          <Button size="sm" disabled={!tenant} onClick={() => update.mutate({ tenantDataSharing: { set: { tenant, level } } })}>
            Set level
          </Button>
        </Card>
      )}

      {s && (
        <Card title="Limits">
          <p>Session budget {s.limits.budgets.session} tokens.</p>
          <p>User day budget {s.limits.budgets.userDay} tokens.</p>
          <p>Tenant day budget {s.limits.budgets.tenantDay} tokens.</p>
          <p>Steps per session {s.limits.budgets.steps}.</p>
          <p>External data sharing {s.limits.externalDataSharing}.</p>
          <p>Approval expiry {s.limits.approvalExpiry}.</p>
        </Card>
      )}

      {s && (
        <Card title="Egress">
          {s.limits.egress.networkPolicy ? (
            <>
              <p>Network policy on. CIDRs: {s.limits.egress.cidrs.join(", ")}. Ports: {s.limits.egress.ports.join(", ")}.</p>
              <p>These are chart values, not a live probe.</p>
            </>
          ) : (
            <p>The BFF egress is not pinned.</p>
          )}
        </Card>
      )}
    </SectionPage>
  );
}
