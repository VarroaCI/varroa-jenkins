import { useState, useMemo, useEffect } from "react";
import { useNavigate, Link, useSearchParams } from "react-router-dom";
import { useCatalogItems } from "../hooks/useCatalog";
import { useComposer } from "../context/ComposerContext";
import { clusterQuery } from "../routing";
import { Button } from "../components/Button";
import ClusterSelector from "../components/ClusterSelector";
import NoAccessibleClusters from "../components/NoAccessibleClusters";
import { useConfigurationCluster } from "../hooks/useConfigurationCluster";
import ComposerTray from "./ComposerTray";
import type { CatalogItemSummary } from "../types";
import type { EditTarget } from "./ComposedBundleEdit";
import {
  UPDATE_CENTER_SOURCE,
  VERDICT_LABEL,
  groupByPlugin,
  isWarningVerdict,
  worstVerdict,
} from "../lib/compat";
import styles from "./CatalogBrowser.module.css";

const TYPE_FILTERS = [
  { value: "", label: "All" },
  { value: "podtemplate", label: "PodTemplates" },
  { value: "plugin", label: "Plugins" },
  { value: "item", label: "Items" },
  { value: "jcasc", label: "JCasC" },
  { value: "rbac", label: "RBAC" },
  { value: "pipeline-template", label: "Pipeline Templates" },
] as const;

const TYPE_ICONS: Record<string, string> = {
  podtemplate: "━",
  plugin: "◆",
  item: "○",
  jcasc: "⚙",
  rbac: "⚇",
  "pipeline-template": "▶",
};

interface CatalogBrowserProps {
  editTarget?: EditTarget | null;
}

export default function CatalogBrowser({ editTarget }: CatalogBrowserProps) {
  const navigate = useNavigate();
  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [typeFilter, setTypeFilter] = useState("");
  const [trayOpen, setTrayOpen] = useState(!!editTarget);
  const composer = useComposer();
  // Standalone browse targets a user-selected cluster; in edit mode the cluster
  // is fixed to the one the composer draft (and its bundle) lives on, so only
  // items that actually exist on the target cluster can be added.
  const [searchParams, setSearchParams] = useSearchParams();
  const context = useConfigurationCluster();
  const cluster = editTarget ? (composer.cluster ?? context.cluster) : context.cluster;

  // Keep the create-composer draft pointed at the browsed cluster so the tray's
  // preview/create/validate calls address it (edit mode seeds cluster itself).
  const setComposerCluster = composer.setCluster;
  useEffect(() => {
    if (!editTarget && cluster) setComposerCluster(cluster);
  }, [editTarget, cluster, setComposerCluster]);

  // Debounced search
  const debounceTimer = useMemo(() => {
    let timer: ReturnType<typeof setTimeout> | null = null;
    return (value: string) => {
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => setDebouncedSearch(value), 250);
    };
  }, []);

  const { data, isLoading, error } = useCatalogItems(cluster, {
    type: typeFilter || undefined,
    q: debouncedSearch || undefined,
  });

  const items = useMemo(() => data?.items ?? [], [data]);

  // Update-center-derived items are rendered as their own group, collapsed by
  // plugin so a plugin stored at three versions is one row with a version
  // selector rather than three rows.
  const ucItems = useMemo(
    () => items.filter((i) => i.sourceRef === UPDATE_CENTER_SOURCE),
    [items],
  );
  const sourceItems = useMemo(
    () => items.filter((i) => i.sourceRef !== UPDATE_CENTER_SOURCE),
    [items],
  );
  const ucGroups = useMemo(() => groupByPlugin(ucItems), [ucItems]);

  // Which version a collapsed row is showing. Unset means the group's default,
  // which groupByPlugin has already put first.
  const [versionChoice, setVersionChoice] = useState<Record<string, string>>({});

  function handleSearchChange(e: React.ChangeEvent<HTMLInputElement>) {
    const value = e.target.value;
    setSearch(value);
    debounceTimer(value);
  }

  function handleCardClick(item: CatalogItemSummary) {
    // In edit mode, clicking a card adds it to the bundle instead of navigating
    // to the standalone item-detail page (which lives outside this editor's
    // composer session and would discard in-progress edits).
    if (editTarget) {
      if (!composer.hasItem({ name: item.name, namespace: item.namespace })) {
        composer.addItem({ name: item.name, namespace: item.namespace, variables: {} });
      }
      return;
    }
    const ns = item.namespace || "default";
    navigate(
      `/catalog/items/${encodeURIComponent(ns)}/${encodeURIComponent(item.name)}${clusterQuery(cluster)}`,
    );
  }

  function handleAddItem(e: React.MouseEvent, item: CatalogItemSummary) {
    e.stopPropagation();
    composer.addItem({ name: item.name, namespace: item.namespace, variables: {} });
  }

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          {editTarget ? (
            <div className={styles.editBanner}>
              <Link
                to={`/catalog/bundles/${editTarget.namespace}/${editTarget.name}${clusterQuery(cluster)}`}
                className={styles.backLink}
                // Leaving via this link is an intentional cancel — discard the
                // in-progress edit draft so the next visit starts from the
                // saved bundle. (Refresh/back-button keep the draft.)
                onClick={() => composer.clearPersisted()}
              >
                &larr; Cancel &amp; back to bundle
              </Link>
              <div className={styles.pageTitle}>
                Editing bundle: {editTarget.baseBundle.spec.displayName || editTarget.name}
              </div>
            </div>
          ) : (
            <>
              <div className={styles.pageTitle}>Catalog Browser</div>
              <div className={styles.pageDesc}>
                Browse and discover catalog items from registered sources
              </div>
            </>
          )}
        </div>
      </div>

      <div className={styles.toolbar}>
        <div className={styles.searchBox}>
          <span>&#x2315;</span>
          <input
            placeholder="Search items..."
            value={search}
            onChange={handleSearchChange}
          />
        </div>
        <select
          className={styles.filterSelect}
          value={typeFilter}
          onChange={(e) => setTypeFilter(e.target.value)}
        >
          {TYPE_FILTERS.map((f) => (
            <option key={f.value} value={f.value}>
              {f.label}
            </option>
          ))}
        </select>
        {/* Standalone browse only: the cluster is fixed in edit mode. */}
        {!editTarget && cluster && (
          <ClusterSelector value={cluster} onChange={(value) => {
            const next = new URLSearchParams(searchParams);
            next.set("cluster", value);
            setSearchParams(next);
          }} />
        )}
      </div>

      {context.ready && !cluster && <NoAccessibleClusters />}
      {error && (
        <div className={styles.errorBanner}>
          Failed to load catalog items: {error.message}
        </div>
      )}

      {isLoading && (
        <div className={styles.loadingBanner}>Loading catalog items...</div>
      )}

      {!isLoading && !error && items.length === 0 && (
        <div className={styles.empty}>
          {debouncedSearch
            ? "No items match your search."
            : "No catalog items found. Register a CatalogSource to get started."}
        </div>
      )}

      {!isLoading && !error && ucGroups.length > 0 && (
        <section className={styles.ucGroup}>
          <div className={styles.groupHeading}>
            <span className={styles.groupTitle}>Update Center</span>
            <span className={styles.groupDesc}>
              Plugins served by the in-cluster update center. Their dependency closures are
              pinned automatically.
            </span>
          </div>
          <div className={styles.grid}>
            {ucGroups.map((group) => {
              const selectedVersion = versionChoice[group.pluginName] ?? group.versions[0].version;
              const item =
                group.versions.find((v) => v.version === selectedVersion) ?? group.versions[0];
              const verdict = worstVerdict(item.compat);
              return (
                <div
                  key={group.pluginName}
                  className={styles.card}
                  onClick={() => handleCardClick(item)}
                >
                  <div className={styles.cardTop}>
                    <span className={styles.typeIcon}>{TYPE_ICONS[item.type] || "○"}</span>
                    <div className={styles.displayName}>{item.displayName || group.pluginName}</div>
                    {!item.valid && <span className={styles.invalidBadge}>&#x26A0; Invalid</span>}
                    {verdict && isWarningVerdict(verdict) && (
                      <span
                        className={styles.compatBadge}
                        data-verdict={verdict}
                        title={
                          item.compat
                            ?.filter((c) => c.verdict === verdict)
                            .map((c) => `${c.profile}: ${c.message || c.verdict}`)
                            .join("\n") || VERDICT_LABEL[verdict]
                        }
                      >
                        &#x26A0; {VERDICT_LABEL[verdict]}
                      </span>
                    )}
                  </div>
                  <div className={styles.path}>{item.type}</div>
                  {item.description && <div className={styles.description}>{item.description}</div>}
                  {item.tags && item.tags.length > 0 && (
                    <div className={styles.tags}>
                      {item.tags.map((tag) => (
                        <span key={tag} className={styles.tag}>
                          {tag}
                        </span>
                      ))}
                    </div>
                  )}
                  <div className={styles.cardFooter}>
                    <span className={styles.sourceBadge}>&#x25C8; {item.sourceRef}</span>
                    {group.versions.length > 1 ? (
                      <select
                        className={styles.versionSelect}
                        aria-label={`Version for ${group.pluginName}`}
                        value={selectedVersion}
                        onClick={(e) => e.stopPropagation()}
                        onChange={(e) => {
                          e.stopPropagation();
                          setVersionChoice((prev) => ({
                            ...prev,
                            [group.pluginName]: e.target.value,
                          }));
                        }}
                      >
                        {group.versions.map((v) => (
                          <option key={v.name} value={v.version}>
                            v{v.version}
                          </option>
                        ))}
                      </select>
                    ) : (
                      item.version && <span className={styles.version}>v{item.version}</span>
                    )}
                    <span onClick={(e) => e.stopPropagation()}>
                      {composer.hasItem({ name: item.name, namespace: item.namespace }) ? (
                        <span className={styles.addedBadge}>&#x2713; Added</span>
                      ) : (
                        /* A compat badge NEVER disables this — derivability blocks,
                           compatibility only advises. */
                        <Button size="sm" variant="ghost" onClick={(e) => handleAddItem(e, item)}>
                          + Add to bundle
                        </Button>
                      )}
                    </span>
                  </div>
                </div>
              );
            })}
          </div>
        </section>
      )}

      {!isLoading && !error && sourceItems.length > 0 && (
        <div className={styles.grid}>
          {sourceItems.map((item) => (
            <div
              key={`${item.namespace}/${item.name}`}
              className={styles.card}
              onClick={() => handleCardClick(item)}
            >
              <div className={styles.cardTop}>
                <span className={styles.typeIcon}>
                  {TYPE_ICONS[item.type] || "○"}
                </span>
                <div className={styles.displayName}>
                  {item.displayName || item.name}
                </div>
                {!item.valid && (
                  <span className={styles.invalidBadge}>
                    &#x26A0; Invalid
                  </span>
                )}
              </div>
              <div className={styles.path}>{item.type}</div>
              {item.description && (
                <div className={styles.description}>{item.description}</div>
              )}
              {item.tags && item.tags.length > 0 && (
                <div className={styles.tags}>
                  {item.tags.map((tag) => (
                    <span key={tag} className={styles.tag}>{tag}</span>
                  ))}
                </div>
              )}
              <div className={styles.cardFooter}>
                <span className={styles.sourceBadge}>
                  &#x25C8; {item.sourceRef}
                </span>
                {item.version && (
                  <span className={styles.version}>v{item.version}</span>
                )}
                <span onClick={(e) => e.stopPropagation()}>
                  {composer.hasItem({ name: item.name, namespace: item.namespace }) ? (
                    <span className={styles.addedBadge}>&#x2713; Added</span>
                  ) : (
                    <Button size="sm" variant="ghost" onClick={(e) => handleAddItem(e, item)}>
                      + Add to bundle
                    </Button>
                  )}
                </span>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* FAB to (re)open the composer tray when it is closed. Available in edit
          mode too, so closing the tray doesn't strand the user without a Save. */}
      {!trayOpen && (composer.items.length > 0 || editTarget) && (
        <button
          className={styles.fab}
          onClick={() => setTrayOpen(true)}
          title={editTarget ? "Open bundle editor" : "Open bundle composer"}
        >
          <span className={styles.fabIcon}>&#x25C8;</span>
          <span className={styles.fabCount}>{composer.items.length}</span>
        </button>
      )}

      {/* Composer tray overlay */}
      <ComposerTray open={trayOpen} onClose={() => setTrayOpen(false)} editTarget={editTarget ?? undefined} />
    </div>
  );
}
