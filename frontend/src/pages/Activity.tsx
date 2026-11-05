import { useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import { useActivityFeed } from "../hooks/useActivityFeed";
import { useControllers } from "../hooks/useControllers";
import ActivityTimeline from "../components/ActivityTimeline";
import type { ActivityFilters } from "../types";
import styles from "./Activity.module.css";

const UNLISTED = "unlisted-placeholder"; // no slash, so it never equals an option value
const FILTERS = ["range", "start", "end", "cluster", "controller", "namespace", "source", "severity", "actor", "type"] as const;

export default function Activity() {
  const [params, setParams] = useSearchParams();
  const { data: controllers = [] } = useControllers();
  const filters = Object.fromEntries(FILTERS.flatMap((key) => params.get(key) ? [[key, params.get(key)!]] : [])) as ActivityFilters;
  const feed = useActivityFeed(undefined, filters);
  const { events, retentionMode, retentionDays } = feed;
  const set = (key: string, value: string) => setParams((current) => { const next = new URLSearchParams(current); value ? next.set(key, value) : next.delete(key); if (key === "range" && value !== "custom") { next.delete("start"); next.delete("end"); }
    // A controller value identifies one controller only with its cluster. The two parameters clear together.
    if (!value && (key === "controller" || key === "cluster")) { next.delete("controller"); next.delete("cluster"); }
    return next; });
  const namespaces = useMemo(() => [...new Set([...controllers.map((c) => c.namespace), ...events.flatMap((e) => e.namespace ? [e.namespace] : [])])].sort(), [controllers, events]);
  const actors = useMemo(() => [...new Set(events.flatMap((e) => e.actor ? [e.actor] : []))].sort(), [events]);
  const types = useMemo(() => [...new Set(events.map((e) => e.type))].sort(), [events]);
  const controllerOptions = useMemo(() => controllers.filter((c) => c.name).map((c) => ({ value: `${c.cluster}/${c.namespace}/${c.name}`, label: `${c.name} (${c.namespace} / ${c.cluster})`, cluster: c.cluster, controller: `${c.namespace}/${c.name}` })), [controllers]);
  const pair = filters.controller ? `${filters.cluster ?? ""}/${filters.controller}` : "";
  // A URL filter that the controller list does not name shows a disabled placeholder. It adds no controller option.
  const unlisted = pair !== "" && !controllerOptions.some((o) => o.value === pair);
  const controllerValue = unlisted ? UNLISTED : pair;
  const controllerChoices = unlisted ? [...controllerOptions, { value: UNLISTED, label: "Not in the controller list", cluster: "", controller: "", disabled: true }] : controllerOptions;
  const selectController = (value: string) => setParams((current) => {
    const next = new URLSearchParams(current);
    const option = controllerOptions.find((o) => o.value === value);
    next.delete("cluster"); next.delete("controller");
    if (option) { if (option.cluster) next.set("cluster", option.cluster); next.set("controller", option.controller); }
    return next;
  });
  const active = FILTERS.filter((key) => params.has(key) && key !== "start" && key !== "end");

  return <div className={styles.page}>
		<div className={styles.pageHead}><div><div className={styles.pageTitle}>Activity</div><div className={styles.pageDesc}>Real-time event feed and searchable operational history</div></div></div>
    <div className={styles.filterBar} aria-label="Activity filters">
      {retentionMode !== "off" && <label className={styles.filterField}>Range<select aria-label="Time range" value={filters.range ?? ""} onChange={(e) => set("range", e.target.value)}><option value="">Recent</option>{["15m","1h","6h","24h","7d","custom"].map((v) => <option key={v} value={v} disabled={v === "7d" && (retentionDays ?? 7) < 7}>{v === "custom" ? "Custom" : v}</option>)}</select></label>}
      {filters.range === "custom" && <><label className={styles.filterField}>Start<input aria-label="Start" type="datetime-local" value={filters.start?.slice(0,16) ?? ""} onChange={(e) => set("start", e.target.value ? new Date(e.target.value).toISOString() : "")} /></label><label className={styles.filterField}>End<input aria-label="End" type="datetime-local" value={filters.end?.slice(0,16) ?? ""} onChange={(e) => set("end", e.target.value ? new Date(e.target.value).toISOString() : "")} /></label></>}
      <Filter label="Controller" value={controllerValue} onChange={selectController} options={controllerChoices} />
      <Filter label="Namespace" value={filters.namespace} onChange={(v) => set("namespace", v)} options={namespaces.map((v) => ({value:v,label:v}))} />
      <Filter label="Source" value={filters.source} onChange={(v) => set("source", v)} options={["operator","mite","jenkins","user","api","mcp"].map((v) => ({value:v,label:v}))} />
      <Filter label="Severity" value={filters.severity} onChange={(v) => set("severity", v)} options={["info","warning","error"].map((v) => ({value:v,label:v}))} />
      <Filter label="Actor" value={filters.actor} onChange={(v) => set("actor", v)} options={actors.map((v) => ({value:v,label:v}))} />
      <Filter label="Event type" value={filters.type} onChange={(v) => set("type", v)} options={types.map((v) => ({value:v,label:v}))} />
      {active.length > 0 && <button className={styles.clearAll} onClick={() => setParams({})}>Clear all</button>}
    </div>
    <div className={styles.activeFilters}>{active.map((key) => <button key={key} onClick={() => set(key, "")}>{key}: {params.get(key)} x</button>)}</div>
    {retentionMode === "off" && <p className={styles.retentionNote}>History retention is off. Showing this server's current activity buffer.</p>}
    <ActivityTimeline feed={feed} />
  </div>;
}

function Filter({label,value,onChange,options}:{label:string;value?:string;onChange:(v:string)=>void;options:{value:string;label:string;disabled?:boolean}[]}) {
  return <label className={styles.filterField}>{label}<select aria-label={label} value={value ?? ""} onChange={(e) => onChange(e.target.value)}><option value="">All</option>{options.map((o) => <option key={o.value} value={o.value} disabled={o.disabled}>{o.label}</option>)}</select></label>;
}
