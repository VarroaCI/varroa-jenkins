import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useVirtualizer } from "@tanstack/react-virtual";
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

const ATTENTION_CHIP = "Needs attention";
const PHASES = ["All", ATTENTION_CHIP, "Connected", "Running", "Provisioning", "Hibernated", "Failed"] as const;

// The kind filter carries no All chip: a chip toggles off, and both chips
// off means every kind.
const KINDS = [
  { value: "cell", label: "Cells" },
  { value: "stateful", label: "Stateful" },
] as const;

// KindCell renders the controller's kind. A cell row also names its pool
// and states its fill as a ratio of reported values. A stateful controller
// holds no pool and no tenant, so its row carries the label alone.
/** Drops the scheme and puts a break chance after each dot, so a narrow
 *  column wraps the host between labels and never inside one. */
function EndpointLabel({ url }: { url: string }) {
  const host = url.replace(/^[a-z][a-z0-9+.-]*:\/\//i, "").replace(/\/$/, "");
  const parts = host.split(".");
  return (
    <>
      {parts.map((part, i) => (
        <Fragment key={i}>
          {part}
          {i < parts.length - 1 && <>.<wbr /></>}
        </Fragment>
      ))}
      {" ↗"}
    </>
  );
}

function KindCell({ c }: { c: ControllerListItem }) {
  if (c.kind !== "cell") {
    return (
      <span className={styles.kindCell} data-label="Kind">
        <span className={styles.kindLabel}>Stateful</span>
      </span>
    );
  }
  return (
    <span className={styles.kindCell} data-label="Kind">
      <span className={styles.kindLabel}>
        Cell
        {c.warm && <span className={styles.warmBadge}>warm</span>}
      </span>
      {c.cellPool && <span className={styles.kindPool}>{c.cellPool}</span>}
      {c.tenantCount != null && c.maxTenants != null && (
        <span className={styles.kindRatio}>{c.tenantCount} / {c.maxTenants}</span>
      )}
    </span>
  );
}

// PickerRow is one entry of the flat list the virtualizer windows. A group
// row carries a header; a controller row carries one controller.
type PickerRow =
  | { type: "group"; key: string; label: string; count: number }
  | { type: "controller"; key: string; c: ControllerListItem };

type SortKey = "name" | "cluster" | "phase" | "kind" | "version" | "endpoint" | "mite";
type SortDir = "asc" | "desc";

function compareControllers(a: ControllerListItem, b: ControllerListItem, key: SortKey): number {
  switch (key) {
    case "cluster":
      return a.cluster.localeCompare(b.cluster) || a.name.localeCompare(b.name);
    case "phase":
      return a.phase.localeCompare(b.phase) || a.name.localeCompare(b.name);
    case "kind":
      return a.kind.localeCompare(b.kind) || a.name.localeCompare(b.name);
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
  const [kindFilter, setKindFilter] = useState<string[]>([]);
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

  const allControllers = useMemo(() => controllers ?? [], [controllers]);
  const filtered = useMemo(() => allControllers.filter((c) => {
    if (phaseFilter === ATTENTION_CHIP) {
      if (!c.attention) return false;
    } else if (phaseFilter !== "All" && c.phase !== phaseFilter) return false;
    if (nameFilter && !c.name.toLowerCase().includes(nameFilter.toLowerCase())) return false;
    if (nsFilter && c.namespace !== nsFilter) return false;
    if (clusterFilter && c.cluster !== clusterFilter) return false;
    if (kindFilter.length > 0 && !kindFilter.includes(c.kind)) return false;
    return true;
  }), [allControllers, phaseFilter, nameFilter, nsFilter, clusterFilter, kindFilter]);

  const sortedFiltered = useMemo(() => [...filtered].sort((a, b) => {
    const cmp = compareControllers(a, b, sortKey);
    return sortDir === "asc" ? cmp : -cmp;
  }), [filtered, sortKey, sortDir]);

  const filteredKeys = useMemo(
    () => filtered.map((c) => c.cluster + "/" + c.namespace + "/" + c.name),
    [filtered],
  );
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

  const counts = useMemo(() => {
    const m: Record<string, number> = {};
    for (const c of allControllers) {
      m[c.phase] = (m[c.phase] ?? 0) + 1;
    }
    m["All"] = allControllers.length;
    m[ATTENTION_CHIP] = allControllers.filter((c) => c.attention).length;
    return m;
  }, [allControllers]);

  const kindCounts = useMemo(() => {
    const m: Record<string, number> = { cell: 0, stateful: 0 };
    for (const c of allControllers) {
      m[c.kind] = (m[c.kind] ?? 0) + 1;
    }
    return m;
  }, [allControllers]);
  const toggleKind = (value: string) => {
    setKindFilter((prev) => (prev.includes(value) ? prev.filter((v) => v !== value) : [...prev, value]));
  };

  const nsCounts = useMemo(() => {
    const m: Record<string, number> = {};
    for (const c of allControllers) {
      m[c.namespace] = (m[c.namespace] ?? 0) + 1;
    }
    return m;
  }, [allControllers]);
  const nsOptions = useMemo(() => Object.keys(nsCounts).sort(), [nsCounts]);

  const clusterOptions = useMemo(
    () => [...new Set(allControllers.map((c) => c.cluster))].sort(),
    [allControllers],
  );

  // rows is the flat list the virtualizer windows: group headers and
  // controller rows in display order.
  const rows = useMemo<PickerRow[]>(() => {
    const controllerRow = (c: ControllerListItem): PickerRow => ({
      type: "controller",
      key: c.cluster + "/" + c.namespace + "/" + c.name,
      c,
    });
    const groupRow = (key: string, label: string, count: number): PickerRow => ({
      type: "group",
      key,
      label,
      count,
    });

    if (groupBy === "none") return sortedFiltered.map(controllerRow);

    if (groupBy === "namespace") {
      const groups: Record<string, ControllerListItem[]> = {};
      for (const c of sortedFiltered) (groups[c.namespace] ??= []).push(c);
      return Object.keys(groups).sort().flatMap((ns) => [
        groupRow(`group-ns-${ns}`, ns, groups[ns].length),
        ...groups[ns].map(controllerRow),
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
      groupRow(`group-cl-${cl}`, cl, groups[cl].length),
      ...groups[cl].map(controllerRow),
    ]);
  }, [sortedFiltered, groupBy, core]);

  // Rows are measured because the mobile layout below 760px renders taller
  // rows. The hook runs before the loading and error branches.
  const scrollRef = useRef<HTMLDivElement>(null);
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: (i) => (rows[i]?.type === "group" ? 36 : 56),
    overscan: 12,
    getItemKey: (i) => rows[i]?.key ?? i,
    measureElement: (el) => el.getBoundingClientRect().height,
  });

  // Rows outside the window are not in the DOM, so Arrow keys move focus
  // between rows and scroll the window to the row that takes focus.
  const pendingFocus = useRef<number | null>(null);
  const focusRow = (index: number): boolean => {
    const el = scrollRef.current?.querySelector<HTMLElement>(`[data-index="${index}"] [role="button"]`);
    if (!el) return false;
    el.focus();
    return true;
  };
  useEffect(() => {
    if (pendingFocus.current != null && focusRow(pendingFocus.current)) {
      pendingFocus.current = null;
    }
  });
  const onListKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const current = (e.target as HTMLElement).closest<HTMLElement>("[data-index]");
    if (!current) return;
    const step = e.key === "ArrowDown" ? 1 : -1;
    let next = Number(current.dataset.index) + step;
    while (next >= 0 && next < rows.length && rows[next].type === "group") next += step;
    if (next < 0 || next >= rows.length) return;
    e.preventDefault();
    if (focusRow(next)) return;
    pendingFocus.current = next;
    virtualizer.scrollToIndex(next, { align: "auto" });
  };

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
          <StatusPill phase={c.phase} attention={c.attention} />
        </span>
        <KindCell c={c} />
        <VersionCell c={c} versions={versions} />
        <span data-label="Endpoint">
          {c.endpoint ? (
            <a
              className={styles.ext}
              href={c.endpoint}
              title={c.endpoint}
              onClick={(e) => e.stopPropagation()}
              target="_blank"
              rel="noreferrer"
            >
              <EndpointLabel url={c.endpoint} />
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
    <div
      ref={scrollRef}
      className={compact ? styles.tableScrollCompact : styles.tableScroll}
      data-testid="controller-table-scroll"
      onKeyDown={onListKeyDown}
    >
      <div className={`${styles.ctrlRow} ${styles.head} ${styles.stickyHead}`}>
        <span>
          {canManage && <input type="checkbox" checked={allFilteredSelected} onChange={toggleAll} onClick={(e) => e.stopPropagation()} />}
        </span>
        <SortHeader label="Controller" sortKeyName="name" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Cluster" sortKeyName="cluster" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Phase" sortKeyName="phase" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Kind" sortKeyName="kind" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Version" sortKeyName="version" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Endpoint" sortKeyName="endpoint" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <SortHeader label="Mite" sortKeyName="mite" activeKey={sortKey} dir={sortDir} onSort={toggleSort} />
        <span />
      </div>
      <div className={styles.virtualBody} style={{ height: virtualizer.getTotalSize() }}>
        {virtualizer.getVirtualItems().map((v) => {
          const row = rows[v.index];
          return (
            <div
              key={v.key}
              data-index={v.index}
              data-last={v.index === rows.length - 1 ? "" : undefined}
              ref={virtualizer.measureElement}
              className={styles.virtualRow}
              style={{ transform: `translateY(${v.start}px)` }}
            >
              {row.type === "group"
                ? <div className={styles.groupHeader}>{row.label} ({row.count})</div>
                : renderRow(row.c)}
            </div>
          );
        })}
      </div>
      {filtered.length === 0 && (
        <div className={styles.empty}>
          {allControllers.length === 0
            ? "No controllers found."
            : "No controllers match the current filters."}
        </div>
      )}
    </div>
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
        <div className={styles.kindChips}>
          {KINDS.map((k) => (
            <button
              key={k.value}
              type="button"
              aria-pressed={kindFilter.includes(k.value)}
              className={`${styles.chip} ${kindFilter.includes(k.value) ? styles.chipOn : ""}`}
              onClick={() => toggleKind(k.value)}
            >
              {k.label} <span className={styles.chipCount}>{kindCounts[k.value] ?? 0}</span>
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
        <div className={styles.filterControls}>
          <select
            value={nsFilter}
            onChange={(e) => setNsFilter(e.target.value)}
            aria-label="Namespace"
            className={`${styles.groupSelect} ${nsFilter ? styles.filterSelected : ""}`}
          >
            <option value="">All namespaces ({allControllers.length})</option>
            {nsOptions.map((ns) => <option key={ns} value={ns}>{ns} ({nsCounts[ns]})</option>)}
          </select>
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
