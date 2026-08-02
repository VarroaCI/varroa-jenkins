import { useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useControllers } from "../hooks/useControllers";
import { useClusters, coreOf } from "../hooks/useClusters";
import { getProvisioningConfig } from "../api/client";
import { upgradeInfo, versionsDiffer } from "../lib/versionCatalog";
import { StatusPill } from "./StatusPill";
import { Pulse } from "./Pulse";
import { Card } from "./Card";
import { useAuth } from "../context/AuthContext";
import { canDoAnywhere } from "../hooks/usePermissions";
import { controllerRoute } from "../routing";
import type { ControllerListItem } from "../hooks/useControllers";
import type { VersionCatalogEntry } from "../types";
import styles from "./BroodControllerPicker.module.css";

const PHASES = ["All", "Connected", "Running", "Provisioning", "Hibernated", "Failed"] as const;

type SortKey = "name" | "cluster" | "phase" | "version" | "endpoint" | "mite";
type SortDir = "asc" | "desc";

function compareControllers(a: ControllerListItem, b: ControllerListItem, key: SortKey): number {
  switch (key) {
    case "cluster":
      return a.cluster.localeCompare(b.cluster) || a.name.localeCompare(b.name);
    case "phase":
      return a.phase.localeCompare(b.phase) || a.name.localeCompare(b.name);
    case "version": {
      const av = a.jenkinsVersion || a.version || "";
      const bv = b.jenkinsVersion || b.version || "";
      return av.localeCompare(bv, undefined, { numeric: true }) || a.name.localeCompare(b.name);
    }
    case "endpoint":
      return (a.endpoint || "").localeCompare(b.endpoint || "") || a.name.localeCompare(b.name);
    case "mite":
      return Number(b.miteConnected) - Number(a.miteConnected) || a.name.localeCompare(b.name);
    case "name":
    default:
      return a.name.localeCompare(b.name);
  }
}

// SortHeader renders a clickable, keyboard-accessible column header that
// toggles asc/desc on click and exposes the active sort via aria-sort so
// screen readers announce it (#438).
function SortHeader({
  label,
  sortKeyName,
  activeKey,
  dir,
  onSort,
}: {
  label: string;
  sortKeyName: SortKey;
  activeKey: SortKey;
  dir: SortDir;
  onSort: (key: SortKey) => void;
}) {
  const active = activeKey === sortKeyName;
  const ariaSort: "ascending" | "descending" | "none" = active
    ? dir === "asc"
      ? "ascending"
      : "descending"
    : "none";
  return (
    <span role="columnheader" aria-sort={ariaSort}>
      <button type="button" className={styles.sortButton} onClick={() => onSort(sortKeyName)}>
        {label}
        <span className={styles.sortIcon} aria-hidden="true">
          {active ? (dir === "asc" ? "▲" : "▼") : "↕"}
        </span>
      </button>
    </span>
  );
}

// VersionCell renders the running version (falling back to desired), a drift
// badge when the two genuinely differ, and upgrade/EOL badges from the catalog.
function VersionCell({ c, versions }: { c: ControllerListItem; versions: VersionCatalogEntry[] }) {
  const shown = c.jenkinsVersion || c.version || "—";
  const drift = versionsDiffer(c.jenkinsVersion, c.version);
  const info = c.version ? upgradeInfo(c.version, versions) : { managed: false };
  return (
    <span className={styles.versionCell} data-label="Version">
      <span className={styles.mono}>{shown}</span>
      {drift && <span className={styles.driftBadge}>→ {c.version}</span>}
      {info.managed && info.recommendedUpgrade && (
        <span className={styles.upgradeBadge}>↑ {info.recommendedUpgrade}</span>
      )}
      {info.managed && info.eol && (
        <span className={info.eolPassed ? styles.eolBadgePast : styles.eolBadge}>EOL {info.eol}</span>
      )}
    </span>
  );
}

interface BroodControllerPickerProps {
  selected: string[];
  onSelectionChange: (keys: string[]) => void;
  /** Renders the table without the outer Card chrome, for embedding inside a dialog. */
  compact?: boolean;
}

export function BroodControllerPicker({ selected, onSelectionChange, compact = false }: BroodControllerPickerProps) {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { data: controllers, isLoading, error } = useControllers();
  const { data: clusters } = useClusters();
  const core = coreOf(clusters)?.name ?? clusters?.[0]?.name ?? "core";
  const { data: cfg } = useQuery({
    queryKey: ["provisioning-config", core],
    queryFn: () => getProvisioningConfig(core),
  });
  const versions = cfg?.versions ?? [];
  const { permissions } = useAuth();
  const canManage = canDoAnywhere(permissions, "controllers", "manage");
  const [phaseFilter, setPhaseFilter] = useState<string>("All");
  const [nameFilter, setNameFilter] = useState("");
  const [nsFilter, setNsFilter] = useState<string>("");
  const [groupBy, setGroupBy] = useState<"none" | "namespace" | "cluster">("none");
  // Default sort is name asc so row order is deterministic across reloads,
  // independent of the multi-cluster fan-out's completion order (#438).
  const [sortKey, setSortKey] = useState<SortKey>("name");
  const [sortDir, setSortDir] = useState<SortDir>("asc");
  const toggleSort = (key: SortKey) => {
    if (key === sortKey) {
      setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    } else {
      setSortKey(key);
      setSortDir("asc");
    }
  };

  // In compact mode this component is embedded in a wizard on another page
  // (BroodOperations.tsx), so the cluster filter must stay local state
  // instead of mutating the host page's URL. Non-compact mode keeps syncing
  // to the URL, but only touches the `cluster` key so other query params
  // survive.
  const [localClusterFilter, setLocalClusterFilter] = useState("");
  const clusterFilter = compact ? localClusterFilter : searchParams.get("cluster") || "";
  const setClusterFilter = (val: string) => {
    if (compact) {
      setLocalClusterFilter(val);
      return;
    }
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev);
      if (val) {
        next.set("cluster", val);
      } else {
        next.delete("cluster");
      }
      return next;
    });
  };

  const selectedSet = new Set(selected);

  const toggleSelected = (key: string) => {
    const next = new Set(selectedSet);
    if (next.has(key)) next.delete(key); else next.add(key);
    onSelectionChange(Array.from(next));
  };

  const allControllers = controllers ?? [];
  const filtered = allControllers.filter((c) => {
    if (phaseFilter !== "All" && c.phase !== phaseFilter) return false;
    if (nameFilter && !c.name.toLowerCase().includes(nameFilter.toLowerCase())) return false;
    if (nsFilter && c.namespace !== nsFilter) return false;
    if (clusterFilter && c.cluster !== clusterFilter) return false;
    return true;
  });

  const sortedFiltered = [...filtered].sort((a, b) => {
    const cmp = compareControllers(a, b, sortKey);
    return sortDir === "asc" ? cmp : -cmp;
  });

  const filteredKeys = filtered.map((c) => c.cluster + "/" + c.namespace + "/" + c.name);
  const allFilteredSelected = filteredKeys.length > 0 && filteredKeys.every((k) => selectedSet.has(k));

  const toggleAll = () => {
    if (allFilteredSelected) {
      const filteredKeySet = new Set(filteredKeys);
      onSelectionChange(selected.filter((k) => !filteredKeySet.has(k)));
    } else {
      const next = new Set(selectedSet);
      for (const k of filteredKeys) next.add(k);
      onSelectionChange(Array.from(next));
    }
  };

  const counts: Record<string, number> = {};
  for (const c of allControllers) {
    counts[c.phase] = (counts[c.phase] ?? 0) + 1;
  }
  counts["All"] = allControllers.length;

  const nsCounts: Record<string, number> = {};
  for (const c of allControllers) {
    nsCounts[c.namespace] = (nsCounts[c.namespace] ?? 0) + 1;
  }
  const nsOptions = Object.keys(nsCounts).sort();

  const clusterOptions = [...new Set(allControllers.map((c) => c.cluster))].sort();

  const renderRow = (c: ControllerListItem) => {
    const key = c.cluster + "/" + c.namespace + "/" + c.name;
    const activateRow = () => {
      if (compact) {
        if (canManage) toggleSelected(key);
      } else if (selectedSet.size > 0) {
        toggleSelected(key);
      } else {
        navigate(controllerRoute(c.cluster, c.namespace, c.name));
      }
    };
    return (
      <div
        key={key}
        className={styles.ctrlRow}
        role="button"
        tabIndex={0}
        aria-label={`Controller ${c.name} (${c.namespace}/${c.cluster})`}
        onClick={activateRow}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            activateRow();
          }
        }}
      >
        <span onClick={canManage ? (e) => { e.stopPropagation(); toggleSelected(key); } : undefined}>
          {canManage && <input type="checkbox" checked={selectedSet.has(key)} readOnly />}
        </span>
        <div className={styles.ctrlName} data-label="Controller">
          <div>
            <b>{c.name}</b>
            <small>ns/{c.namespace}</small>
          </div>
        </div>
        <span className={styles.mono} data-label="Cluster">{c.cluster}</span>
        <span data-label="Phase">
          <StatusPill phase={c.phase} />
        </span>
        <VersionCell c={c} versions={versions} />
        <span data-label="Endpoint">
          {c.endpoint ? (
            <a
              className={styles.ext}
              href={c.endpoint}
              onClick={(e) => e.stopPropagation()}
              target="_blank"
              rel="noreferrer"
            >
              {c.endpoint} ↗
            </a>
          ) : (
            <span className={styles.monoMuted}>—</span>
          )}
        </span>
        <span className={styles.miteCell} data-label="Mite">
          <Pulse
            active={c.miteConnected}
            size={10}
            label={c.miteConnected ? "mite connected" : "mite disconnected"}
          />
          <span className={c.miteConnected ? styles.miteTextOn : styles.miteTextOff}>
            {c.miteConnected ? "Connected" : "Disconnected"}
          </span>
        </span>
        <span className={styles.chevron}>›</span>
      </div>
    );
  };

  const tableContent = (
    <>
      <div className={styles.ctrlRow + " " + styles.head}>
        <span>
          {canManage && <input type="checkbox" checked={allFilteredSelected} onChange={toggleAll} onClick={(e) => e.stopPropagation()} />}
        </span>
        <SortHeader label="Controller" sortKeyName="name" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Cluster" sortKeyName="cluster" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Phase" sortKeyName="phase" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Version" sortKeyName="version" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Endpoint" sortKeyName="endpoint" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Mite" sortKeyName="mite" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <span />
      </div>
      {(() => {
        if (groupBy === "none") {
          return sortedFiltered.map(renderRow);
        }

        if (groupBy === "namespace") {
          const groups: Record<string, ControllerListItem[]> = {};
          for (const c of sortedFiltered) (groups[c.namespace] ??= []).push(c);
          const groupNames = Object.keys(groups).sort();
          return groupNames.flatMap((ns) => [
            <div key={`group-ns-${ns}`} className={styles.groupHeader}>
              {ns} ({groups[ns].length})
            </div>,
            ...groups[ns].map(renderRow),
          ]);
        }

        const groups: Record<string, ControllerListItem[]> = {};
        for (const c of sortedFiltered) (groups[c.cluster] ??= []).push(c);
        const groupKeys = Object.keys(groups).sort((a, b) => {
          if (core === a) return -1;
          if (core === b) return 1;
          return a.localeCompare(b);
        });
        return groupKeys.flatMap((cl) => [
          <div key={`group-cl-${cl}`} className={styles.groupHeader}>
            {cl} ({groups[cl].length})
          </div>,
          ...groups[cl].map(renderRow),
        ]);
      })()}
      {filtered.length === 0 && (
        <div className={styles.empty}>
          {allControllers.length === 0
            ? "No controllers found."
            : "No controllers match the current filters."}
        </div>
      )}
    </>
  );

  return (
    <div>
      <div className={styles.filterBar}>
        <div className={styles.chips}>
          {PHASES.map((p) => (
            <button
              key={p}
              className={`${styles.chip} ${phaseFilter === p ? styles.chipOn : ""}`}
              onClick={() => setPhaseFilter(p)}
            >
              {p} <span className={styles.chipCount}>{counts[p] ?? 0}</span>
            </button>
          ))}
        </div>
        <div className={styles.searchBox}>
          <span>⌕</span>
          <input
            placeholder="Filter by name..."
            value={nameFilter}
            onChange={(e) => setNameFilter(e.target.value)}
          />
        </div>
				<label className={styles.filterField}>Namespace
					<select value={nsFilter} onChange={(e) => setNsFilter(e.target.value)} aria-label="Namespace" className={nsFilter ? styles.filterSelected : ""}>
						<option value="">All namespaces ({allControllers.length})</option>
						{nsOptions.map((ns) => <option key={ns} value={ns}>{ns} ({nsCounts[ns]})</option>)}
					</select>
				</label>
        {clusterOptions.length > 0 && (
          <select value={clusterFilter} onChange={(e) => setClusterFilter(e.target.value)} aria-label="Cluster" className={styles.groupSelect}>
            <option value="">All clusters</option>
            {clusterOptions.map((cl) => (
              <option key={cl} value={cl}>{cl}{cl === core ? " (core)" : ""}</option>
            ))}
          </select>
        )}
        <label className={styles.groupToggle}>
          <select
            value={groupBy}
            onChange={(e) => setGroupBy(e.target.value as "none" | "namespace" | "cluster")}
            aria-label="Group by"
            className={styles.groupSelect}
          >
            <option value="none">No grouping</option>
            <option value="namespace">Namespace</option>
            <option value="cluster">Cluster</option>
          </select>
        </label>
      </div>

      {isLoading && (
        <div className={styles.loadingBanner}>Loading controllers...</div>
      )}
      {error && (
        <div className={styles.errorBanner}>Failed to load: {error.message}</div>
      )}

      {!isLoading && !error && (
        compact ? <div>{tableContent}</div> : <Card>{tableContent}</Card>
      )}
    </div>
  );
}
