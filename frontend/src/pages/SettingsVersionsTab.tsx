import { useEffect, useState } from "react";
import {
  getVersionProfiles,
  getProvisioningDefaults,
  listVersionCandidates,
  promoteVersionCandidate,
} from "../api/client";
import type { VersionProfileDetail, VersionProfileCondition, VersionCandidate } from "../types";
import { ApiError } from "../hooks/useApi";
import LoadingSpinner from "../components/LoadingSpinner";
import ClusterSelector from "../components/ClusterSelector";
import { Button } from "../components/Button";
import styles from "./settings.module.css";

function findCondition(
  conditions: VersionProfileCondition[],
  type: string,
): VersionProfileCondition | undefined {
  return conditions.find((c) => c.type === type);
}

const CANDIDATE_CONDITION_TYPES = [
  "Resolved",
  "ClosureClean",
  "CoreCompatOK",
  "PluginsServable",
  "PreflightChecked",
  "Promoted",
] as const;

function ConditionChip({ type, condition }: { type: string; condition: VersionProfileCondition | undefined }) {
  const status = condition?.status ?? "Unknown";
  const cls = status === "True" ? styles.conditionTrue : status === "False" ? styles.conditionFalse : styles.conditionUnknown;
  return (
    <span className={`${styles.conditionChip} ${cls}`} title={condition?.message || condition?.reason || undefined}>
      {type}
    </span>
  );
}

function PromoteConfirmDialog({
  candidate,
  line,
  cluster,
  onClose,
  onPromoted,
  onConflict,
}: {
  candidate: VersionCandidate;
  line: string;
  cluster: string;
  onClose: () => void;
  onPromoted: () => void;
  onConflict: () => void;
}) {
  const [policy, setPolicy] = useState<"auto" | "manual" | null>(null);
  const [promoting, setPromoting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getProvisioningDefaults(cluster)
      .then((d) => {
        if (!cancelled) setPolicy(d.spec.upgradePolicy === "manual" ? "manual" : "auto");
      })
      .catch(() => {
        if (!cancelled) setPolicy("auto");
      });
    return () => {
      cancelled = true;
    };
  }, [cluster]);

  const body =
    policy === "manual"
      ? `In ${cluster}, this holds controllers pinned to line ${line} with UpgradePending until released. Other clusters follow their own upgrade policy.`
      : `In ${cluster}, this rolls controllers pinned to line ${line} through the version-roll gate. Other clusters follow their own upgrade policy.`;

  const confirm = () => {
    setPromoting(true);
    setError(null);
    promoteVersionCandidate(candidate.metadata.name)
      .then(() => onPromoted())
      .catch((e) => {
        if (e instanceof ApiError && e.status === 409) {
          setError("This candidate is no longer available for promotion — the list has been refreshed.");
          onConflict();
        } else {
          setError(e instanceof Error ? e.message : String(e));
        }
      })
      .finally(() => setPromoting(false));
  };

  return (
    <div className={styles.candidateOverlay} onClick={onClose}>
      <div
        className={styles.candidateDialog}
        role="alertdialog"
        aria-label="Confirm promotion"
        onClick={(e) => e.stopPropagation()}
      >
        {policy === null ? <LoadingSpinner /> : <p>{body}</p>}
        {error && <p className={styles.formError}>{error}</p>}
        <div className={styles.dialogActions}>
          <Button onClick={onClose} disabled={promoting}>Cancel</Button>
          <Button variant="primary" onClick={confirm} disabled={promoting || policy === null}>
            {promoting ? "Promoting…" : "Promote"}
          </Button>
        </div>
      </div>
    </div>
  );
}

export default function SettingsVersionsTab() {
  const [profiles, setProfiles] = useState<VersionProfileDetail[] | null>(null);
  const [candidates, setCandidates] = useState<VersionCandidate[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [selectedCluster, setSelectedCluster] = useState("core");
  const [promoteTarget, setPromoteTarget] = useState<VersionCandidate | null>(null);

  const load = () => {
    setLoading(true);
    setError(null);
    Promise.all([getVersionProfiles(selectedCluster), listVersionCandidates()])
      .then(([p, c]) => {
        setProfiles(p);
        setCandidates(c);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)))
      .finally(() => setLoading(false));
  };

  useEffect(load, [selectedCluster]);

  if (loading && !promoteTarget) return <LoadingSpinner />;
  if (error) return <div className={styles.errorBanner}>Error: {error}</div>;
  if (!profiles || profiles.length === 0) {
    return <p className={styles.muted}>No version profiles installed</p>;
  }

  const now = new Date();
  const profileNames = new Set(profiles.map((p) => p.name));
  const clusterCandidates = (candidates ?? []).filter((c) => profileNames.has(c.spec.profileRef));
  const lineFor = (c: VersionCandidate) =>
    profiles.find((p) => p.name === c.spec.profileRef)?.version ?? c.spec.profileRef;
  const active = clusterCandidates.filter(
    (c) => c.status.phase === "Pending" || c.status.phase === "Ready" || c.status.phase === "Failed",
  );
  const history = clusterCandidates.filter(
    (c) => c.status.phase === "Promoted" || c.status.phase === "Superseded",
  );
  const allActive = (candidates ?? []).filter(
    (c) => c.status.phase === "Pending" || c.status.phase === "Ready" || c.status.phase === "Failed",
  );

  return (
    <div>
      <p className={`${styles.muted} ${styles.pageNote}`}>
        Version profiles are read-only in this UI and scoped to the selected cluster. Each pins a plugin set and optional
        JCasC overlay to a Jenkins version or LTS line.
      </p>
      <ClusterSelector value={selectedCluster} onChange={setSelectedCluster} />
      <div className={styles.profileTableWrap}>
        <table className={styles.table}>
          <thead>
            <tr>
              <th>Version</th><th>Channel</th><th>Plugins</th><th>Plugin set</th><th>JCasC</th><th>Materialized</th>
            </tr>
          </thead>
          <tbody>
            {profiles.map((p) => {
              const metadataOnly = !p.pluginSetRef;
              const eolPast = p.eol ? new Date(p.eol) < now : false;
              const ready = findCondition(p.conditions, "PluginSetReady");
              const mismatch = findCondition(p.conditions, "LockJcascMismatch");
              const mismatchActive = mismatch?.status === "True";

              return (
                <tr key={p.name}>
                  <td>
                    <div className={styles.profileBadges}>
                      <span className={styles.profileVersion}>{p.version}</span>
                      {p.recommended && (
                        <span
                          className={`${styles.tableBadge} ${styles.recommended}`}
                        >
                          recommended
                        </span>
                      )}
                      {p.eol && (
                        <span
                          className={`${styles.tableBadge} ${eolPast ? styles.eol : ""}`}
                        >
                          EOL {p.eol}
                        </span>
                      )}
                    </div>
                  </td>
                  <td>
                    <span className={styles.tableBadge}>
                      {p.channel}
                    </span>
                  </td>
                  <td>{metadataOnly ? "—" : p.pluginCount ?? "—"}</td>
                  <td>
                    {metadataOnly ? (
                      "—"
                    ) : ready?.status === "True" ? (
                      <span className={styles.ready} title={ready.message}>
                        Ready
                      </span>
                    ) : (
                      <span className={styles.notReady} title={ready?.message}>
                        Error{ready?.reason ? ` (${ready.reason})` : ""}
                      </span>
                    )}
                  </td>
                  <td>
                    <div className={styles.profileBadges}>
                      {p.hasJcasc ? (
                        <span className={styles.tableBadge}>
                          overlay
                        </span>
                      ) : (
                        "—"
                      )}
                      {mismatchActive && (
                        <span
                          className={`${styles.tableBadge} ${styles.eol}`}
                          title={mismatch?.message}
                        >
                          lock mismatch
                        </span>
                      )}
                    </div>
                  </td>
                  <td>
                    {p.contentRef ? (
                      <code className={styles.profileCode}>{p.contentRef}</code>
                    ) : (
                      "—"
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <h3 className={styles.sectionTitle}>Upgrade candidates</h3>
      {active.length === 0 ? (
        allActive.length > 0 ? (
          <p className={styles.muted}>
            Upgrade candidates aren&apos;t tracked per cluster, and none of the current candidates match this
            cluster&apos;s version profiles.
          </p>
        ) : (
          <p className={styles.muted}>No pending upgrade candidates.</p>
        )
      ) : (
        <div className={styles.candidateTableWrap}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th>Target profile / line</th>
                <th>Discovered version</th>
                <th>Phase</th>
                <th>Conditions</th>
                <th>Pre-flight</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {active.map((c) => {
                const conditions = c.status.conditions ?? [];
                const preflight = c.status.preflight;
                const failing = preflight?.failingControllers ?? [];
                return (
                  <tr key={c.metadata.name}>
                    <td>
                      <div className={styles.profileVersion}>{lineFor(c)}</div>
                      <div className={styles.smallMuted}>{c.spec.profileRef}</div>
                    </td>
                    <td>{c.spec.observedVersion} → {c.spec.resolveVersion}</td>
                    <td>
                      <span className={styles.tableBadge}>{c.status.phase ?? "—"}</span>
                    </td>
                    <td>
                      <div className={styles.profileBadges}>
                        {CANDIDATE_CONDITION_TYPES.map((t) => (
                          <ConditionChip key={t} type={t} condition={findCondition(conditions, t)} />
                        ))}
                      </div>
                    </td>
                    <td>
                      {preflight ? (
                        <>
                          <div className={styles.smallMuted}>
                            {preflight.controllersFailing} of {preflight.controllersChecked} controllers failing
                          </div>
                          {failing.length > 0 && (
                            <ul className={styles.preflightList}>
                              {failing.map((f) => (
                                <li key={`${f.namespace}/${f.name}`} className={styles.preflightItem}>
                                  {f.namespace}/{f.name} — {f.message}
                                </li>
                              ))}
                            </ul>
                          )}
                        </>
                      ) : (
                        "—"
                      )}
                    </td>
                    <td>
                      <Button size="sm" disabled={c.status.phase !== "Ready"} onClick={() => setPromoteTarget(c)}>
                        Promote
                      </Button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {history.length > 0 && (
        <details className={styles.candidateHistory}>
          <summary>History ({history.length})</summary>
          <ul>
            {history.map((c) => (
              <li key={c.metadata.name} className={styles.smallMuted}>
                {lineFor(c)}: {c.spec.observedVersion} → {c.spec.resolveVersion} — {c.status.phase}
                {c.status.promotedAt ? ` (${c.status.promotedAt})` : ""}
              </li>
            ))}
          </ul>
        </details>
      )}

      {promoteTarget && (
        <PromoteConfirmDialog
          candidate={promoteTarget}
          line={lineFor(promoteTarget)}
          cluster={selectedCluster}
          onClose={() => setPromoteTarget(null)}
          onPromoted={() => {
            setPromoteTarget(null);
            load();
          }}
          onConflict={load}
        />
      )}
    </div>
  );
}
