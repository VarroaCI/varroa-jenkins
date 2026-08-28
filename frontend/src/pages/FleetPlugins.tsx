import { useSearchParams } from "react-router-dom";
import { useFleetPluginsRollup, useFleetPluginDrilldown } from "../hooks/useFleetPlugins";
import type { FleetPluginRollupItem, FleetPluginDrillItem, FleetPluginCoverage } from "../types";
import { Card } from "../components/Card";
import styles from "./FleetPlugins.module.css";

export default function FleetPlugins() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const affected = params.get("affected") ?? "";
  const plugin = params.get("plugin") ?? null;
  const cluster = params.get("cluster") ?? undefined;
  const namespace = params.get("namespace") ?? undefined;

  const apiParams = {
    q: q || undefined,
    affected: affected || undefined,
    cluster,
    namespace,
  };

  const { data: rollup, isLoading, error } = useFleetPluginsRollup(apiParams);
  const { data: drilldown, isLoading: drillLoading } = useFleetPluginDrilldown(plugin, apiParams);

  const set = (key: string, value: string) =>
    setParams((current) => {
      const next = new URLSearchParams(current);
      if (value) next.set(key, value);
      else next.delete(key);
      return next;
    });

  const selectPlugin = (name: string) =>
    setParams((current) => {
      const next = new URLSearchParams(current);
      if (next.get("plugin") === name) {
        next.delete("plugin");
      } else {
        next.set("plugin", name);
      }
      return next;
    });

  const coverage = rollup?.coverage;
  const items = rollup?.items ?? [];
  const drillItems = drilldown?.items ?? [];
  const drillVersions = drilldown?.versions ?? [];

  // 502 error state
  if (error) {
    return (
      <div className={styles.page}>
        <div className={styles.pageHead}>
          <div>
            <div className={styles.pageTitle}>Plugins</div>
            <div className={styles.pageDesc}>Fleet-wide installed plugin inventory</div>
          </div>
        </div>
        <div className={styles.errorState}>
          <p>Plugin inventory is not available.</p>
          <p className={styles.errorDetail}>The backend dependency is not wired.</p>
        </div>
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>Plugins</div>
          <div className={styles.pageDesc}>Fleet-wide installed plugin inventory</div>
        </div>
      </div>

      {/* Filter bar */}
      <div className={styles.filterBar}>
        <input
          type="text"
          className={styles.filterInput}
          placeholder="Plugin name…"
          aria-label="Plugin name filter"
          value={q}
          onChange={(e) => set("q", e.target.value)}
        />
        <input
          type="text"
          className={styles.filterInput}
          placeholder="Version range (e.g. <=4.0.0)"
          aria-label="Version range filter"
          value={affected}
          onChange={(e) => set("affected", e.target.value)}
        />
      </div>

      {/* Coverage notice */}
      {coverage && <CoverageNotice coverage={coverage} clusters={rollup?.clusters} />}

      {/* Loading */}
      {isLoading && <div className={styles.loading}>Loading…</div>}

      {/* Empty state */}
      {!isLoading && items.length === 0 && (
        <div className={styles.empty}>
          {coverage && !coverage.complete ? (
            <p className={styles.emptyPartial}>
              No matches among the controllers we could see.
            </p>
          ) : (
            <p>No plugins found.</p>
          )}
        </div>
      )}

      {/* Rollup table */}
      {!isLoading && items.length > 0 && (
        <Card>
          <div className={styles.tableWrapper}>
            <table className={styles.table}>
              <thead>
                <tr>
                  <th>Plugin</th>
                  <th>Controllers</th>
                  <th>Versions</th>
                  <th>Classes</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <RollupRow
                    key={item.name}
                    item={item}
                    selected={plugin === item.name}
                    onSelect={() => selectPlugin(item.name)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}

      {/* Drilldown panel */}
      {plugin && (
        <div className={styles.drilldown}>
          <h2 className={styles.drillTitle}>{plugin}</h2>
          {drillLoading && <div className={styles.loading}>Loading…</div>}
          {!drillLoading && drillItems.length === 0 && (
            <p className={styles.empty}>No controllers report this plugin.</p>
          )}
          {!drillLoading && drillItems.length > 0 && (
            <>
              <div className={styles.drillVersions}>
                <strong>Versions: </strong>
                {drillVersions.map((v, i) => (
                  <span key={v.version}>
                    {i > 0 && ", "}
                    {v.version} ({v.controllerCount})
                  </span>
                ))}
              </div>
              <div className={styles.tableWrapper}>
                <table className={styles.table}>
                  <thead>
                    <tr>
                      <th>Controller</th>
                      <th>Namespace</th>
                      <th>Cluster</th>
                      <th>Version</th>
                      <th>Class</th>
                      <th>Qualifiers</th>
                    </tr>
                  </thead>
                  <tbody>
                    {drillItems.map((di) => (
                      <DrillRow key={`${di.namespace}/${di.controller}`} item={di} />
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}
        </div>
      )}
    </div>
  );
}

function RollupRow({ item, selected, onSelect }: { item: FleetPluginRollupItem; selected: boolean; onSelect: () => void }) {
  return (
    <tr
      className={`${styles.rollupRow} ${selected ? styles.rollupRowSelected : ""}`}
      onClick={onSelect}
      role="button"
      tabIndex={0}
      aria-expanded={selected}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
    >
      <td className={styles.nameCell}>{item.name}</td>
      <td className={styles.countCell}>{item.controllerCount}</td>
      <td className={styles.breakdownCell}>
        {item.versions.map((v) => (
          <span key={v.version} className={styles.versionTag}>
            {v.version} <span className={styles.countBadge}>{v.controllerCount}</span>
          </span>
        ))}
      </td>
      <td className={styles.breakdownCell}>
        {item.classes.map((c) => (
          <span key={c.class} className={styles.classTag}>
            {c.class} <span className={styles.countBadge}>{c.controllerCount}</span>
          </span>
        ))}
      </td>
    </tr>
  );
}

function DrillRow({ item }: { item: FleetPluginDrillItem }) {
  return (
    <tr>
      <td>
        <a href={item.detailPath} className={styles.detailLink}>
          {item.controller}
        </a>
      </td>
      <td>{item.namespace}</td>
      <td>{item.cluster}</td>
      <td className={styles.mono}>{item.version}</td>
      <td className={styles.mono}>{item.class}</td>
      <td className={styles.qualifiers}>
        {item.stale && <span className={styles.qualifierBadge}>stale</span>}
        {item.degraded && <span className={styles.qualifierBadge}>degraded</span>}
        {item.truncated && <span className={styles.qualifierBadge}>truncated</span>}
        {item.optionalEdgesDropped && <span className={styles.qualifierBadge}>edges dropped</span>}
        {item.bootstrapApproximate && <span className={styles.qualifierBadgeApprox}>approx</span>}
        {item.detailStale && <span className={styles.qualifierBadgeStale}>detail stale</span>}
      </td>
    </tr>
  );
}

function CoverageNotice({ coverage, clusters }: { coverage: FleetPluginCoverage; clusters?: Array<{ name: string; ok: boolean; error?: string }> }) {
  const missing = coverage.controllersMissing ?? [];
  const showForMissing = missing.length > 0;
  const showForIncomplete = !coverage.complete;

  if (!showForMissing && !showForIncomplete) return null;

  // Group missing controllers by reason
  const reasonCounts: Record<string, number> = {};
  for (const m of missing) {
    reasonCounts[m.reason] = (reasonCounts[m.reason] ?? 0) + 1;
  }

  const notCovered = clusters?.filter((c) => !c.ok) ?? [];

  return (
    <div className={styles.coverageNotice} role="status" aria-label="Coverage notice">
      <p className={styles.coverageText}>
        {showForIncomplete && (
          <>
            This release covers the local cluster only.{" "}
            {notCovered.length > 0 && (
              <>
                Not covered: {notCovered.map((c) => c.name).join(", ")}.{" "}
              </>
            )}
            Results are limited to controllers we could see.
          </>
        )}
        {showForMissing && !showForIncomplete && "Some controllers have no observed inventory."}
        {showForMissing && (
          <>
            {" "}
            {missing.length} controller{missing.length !== 1 ? "s" : ""} absent
            {Object.entries(reasonCounts).length > 0 && (
              <>: {Object.entries(reasonCounts).map(([r, c]) => `${c} ${r}`).join(", ")}</>
            )}
            .
          </>
        )}
        {coverage.controllersDetailStale > 0 && (
          <> {coverage.controllersDetailStale} with detail-stale classification.</>
        )}
      </p>
    </div>
  );
}
