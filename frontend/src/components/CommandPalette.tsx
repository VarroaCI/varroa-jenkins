import { useDeferredValue, useMemo, useState, useEffect, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Activity, Boxes, Folder, PlayCircle, Server, UsersRound, Workflow } from "lucide-react";
import { bffFetch } from "../hooks/useApi";
import type { ClusterFanoutStatus } from "../hooks/useControllers";
import styles from "./CommandPalette.module.css";

interface SearchResult {
  type: "controller" | "namespace" | "group" | "catalogitem" | "job" | "run" | "activity";
  cluster?: string;
  name: string;
  namespace?: string;
  description?: string;
  link: string;
}

interface SearchBody {
  items: SearchResult[];
  clusters: ClusterFanoutStatus[];
}

interface CommandPaletteProps {
  open?: boolean;
  query?: string;
  onOpenChange?: (open: boolean) => void;
  onQueryChange?: (query: string) => void;
  /** onAsk opens the assistant with the query as its first message. */
  onAsk?: (text: string) => void;
}

const categoryLabels = ["Controllers", "Namespaces", "Groups", "Catalog Items", "Jobs", "Runs", "Activity"];
const groupLabels: Record<SearchResult["type"], string> = {
  controller: "controllers",
  namespace: "namespaces",
  group: "groups",
  catalogitem: "catalog items",
  job: "jobs",
  run: "runs",
  activity: "activity",
};
const icons = {
  controller: Server,
  namespace: Folder,
  group: UsersRound,
  catalogitem: Boxes,
  job: Workflow,
  run: PlayCircle,
  activity: Activity,
};

export function CommandPalette(props: CommandPaletteProps) {
  const [localOpen, setLocalOpen] = useState(false);
  const [localQuery, setLocalQuery] = useState("");
  const open = props.open ?? localOpen;
  const query = props.query ?? localQuery;
  const onOpenChange = props.onOpenChange ?? setLocalOpen;
  const onQueryChange = props.onQueryChange ?? setLocalQuery;
  const [selected, setSelected] = useState(0);
  const deferredQuery = useDeferredValue(query.trim());
  const inputRef = useRef<HTMLInputElement>(null);
  const navigate = useNavigate();

  useEffect(() => {
    if (props.open !== undefined) return;
    const handleKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setLocalOpen((value) => !value);
      } else if (event.key === "Escape") {
        setLocalOpen(false);
      }
    };
    document.addEventListener("keydown", handleKey);
    return () => document.removeEventListener("keydown", handleKey);
  }, [props.open]);

  const { data, isLoading, error } = useQuery({
    queryKey: ["search", deferredQuery],
    queryFn: ({ signal }) => bffFetch<SearchBody>(`/search?q=${encodeURIComponent(deferredQuery)}`, { signal }),
    enabled: open && deferredQuery.length > 0,
    staleTime: 5_000,
  });
  const results = useMemo(() => data?.items ?? [], [data]);
  const failedClusters = useMemo(() => (data?.clusters ?? []).filter((c) => !c.ok), [data]);

  useEffect(() => {
    if (open) {
      inputRef.current?.focus();
    }
  }, [open]);

  useEffect(() => {
    setSelected(0);
  }, [results]);

  const go = (link: string) => {
    onOpenChange(false);
    navigate(link);
  };

  // The "Ask the assistant" row is selectable only once the search answered.
  const askRow = !!props.onAsk && query.trim() !== "" && !isLoading;
  const total = results.length + (askRow ? 1 : 0);

  const handleKey = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setSelected((s) => Math.min(s + 1, total - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setSelected((s) => Math.max(s - 1, 0));
    } else if (e.key === "Enter") {
      if (isLoading) return;
      if (askRow && selected === results.length) {
        onOpenChange(false);
        props.onAsk?.(query.trim());
      } else if (results[selected]) {
        go(results[selected].link);
      }
    }
  };

  if (!open) return null;

  const byType: Record<string, SearchResult[]> = {};
  for (const r of results) {
    (byType[r.type] ??= []).push(r);
  }

  return (
    <div className={styles.overlay} onClick={() => onOpenChange(false)} role="presentation">
      <div className={styles.palette} onClick={(e) => e.stopPropagation()} role="dialog" aria-modal="true" aria-label="Global search">
        <div className={styles.inputRow}>
          <span>⌕</span>
          <input
            ref={inputRef}
            aria-label="Search query"
            placeholder="Search controllers, groups, templates..."
            value={query}
            onChange={(e) => onQueryChange(e.target.value)}
            onKeyDown={handleKey}
          />
          <span className={styles.kbd}>esc</span>
        </div>
        <div className={styles.results}>
          {!query.trim() && <div className={styles.empty}>Type to search<div>{categoryLabels.join(" · ")}</div></div>}
          {isLoading && <div className={styles.empty}>Searching...</div>}
          {error && <div className={styles.empty} role="alert">Search is unavailable. Try again.</div>}
          {query.trim() && !isLoading && !error && failedClusters.map((c) => (
            <div key={c.name} className={styles.notice} role="status">
              Results from cluster <code>{c.name}</code> may be incomplete: {c.error || "the cluster did not answer"}.
            </div>
          ))}
          {Object.entries(byType).map(([type, items]) => (
            <div key={type} className={styles.group}>
              <div className={styles.groupLabel}>{groupLabels[type as SearchResult["type"]] ?? type}</div>
              {items.map((r) => {
                const idx = results.indexOf(r);
                const Icon = icons[r.type];
                return (
                  <button
                    key={`${r.type}/${r.cluster ?? ""}/${r.namespace ?? ""}/${r.name}`}
                    className={`${styles.item} ${idx === selected ? styles.selected : ""}`}
                    onClick={() => go(r.link)}
                    onMouseEnter={() => setSelected(idx)}
                  >
                    <span className={styles.itemType}>
                      <Icon aria-hidden="true" size={18} />
                    </span>
                    <div>
                      <div className={styles.itemName}>{r.name}</div>
                      <div className={styles.itemMeta}>{[r.cluster, r.namespace, r.description].filter(Boolean).join(" / ")}</div>
                    </div>
                  </button>
                );
              })}
            </div>
          ))}
          {query.trim() && !isLoading && !error && results.length === 0 && (
            <div className={styles.empty}>No results for "{query}"</div>
          )}
          {askRow && (
            <div className={styles.results}>
              <button
                type="button"
                className={`${styles.item} ${selected === results.length ? styles.selected : ""}`}
                onMouseEnter={() => setSelected(results.length)}
                onClick={() => {
                  onOpenChange(false);
                  props.onAsk?.(query.trim());
                }}
              >
                <div className={styles.itemName}>Ask the assistant: {query.trim()}</div>
              </button>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
