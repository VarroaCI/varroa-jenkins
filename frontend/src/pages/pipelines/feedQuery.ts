import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";
import type { SortOrder } from "../../components/SortableHeader";
import type { FleetRunSort } from "../../types";

/**
 * FeedQuery is the whole view of the fleet run feed. Every field lives in
 * the URL query, so a link reproduces the view.
 */
export interface FeedQuery {
  tenant: string[];
  job: string;
  trigger: string;
  status: string;
  result: string;
  branch: string;
  actor: string;
  since: string;
  q: string;
  sort: FleetRunSort;
  order: SortOrder;
}

export const DEFAULT_FEED_QUERY: FeedQuery = {
  tenant: [],
  job: "",
  trigger: "",
  status: "",
  result: "",
  branch: "",
  actor: "",
  since: "",
  q: "",
  sort: "started",
  order: "desc",
};

const SORTS: readonly FleetRunSort[] = ["started", "duration", "tenant", "job", "status", "actor"];
const ORDERS: readonly SortOrder[] = ["asc", "desc"];

/** parseFeedQuery reads the view from a URL query. An unknown sort or order takes the default. */
export function parseFeedQuery(sp: URLSearchParams): FeedQuery {
  const sort = sp.get("sort") ?? "";
  const order = sp.get("order") ?? "";
  return {
    tenant: sp.getAll("tenant").filter((t) => t !== ""),
    job: sp.get("job") ?? "",
    trigger: sp.get("trigger") ?? "",
    status: sp.get("status") ?? "",
    result: sp.get("result") ?? "",
    branch: sp.get("branch") ?? "",
    actor: sp.get("actor") ?? "",
    since: sp.get("since") ?? "",
    q: sp.get("q") ?? "",
    sort: (SORTS as readonly string[]).includes(sort) ? (sort as FleetRunSort) : DEFAULT_FEED_QUERY.sort,
    order: (ORDERS as readonly string[]).includes(order) ? (order as SortOrder) : DEFAULT_FEED_QUERY.order,
  };
}

/** writeFeedQuery writes the view as a URL query. It omits defaults and empty values. */
export function writeFeedQuery(q: FeedQuery): URLSearchParams {
  const sp = new URLSearchParams();
  for (const t of q.tenant) sp.append("tenant", t);
  const scalars: [string, string][] = [
    ["job", q.job],
    ["trigger", q.trigger],
    ["status", q.status],
    ["result", q.result],
    ["branch", q.branch],
    ["actor", q.actor],
    ["since", q.since],
    ["q", q.q],
  ];
  for (const [key, value] of scalars) {
    if (value !== "") sp.set(key, value);
  }
  if (q.sort !== DEFAULT_FEED_QUERY.sort) sp.set("sort", q.sort);
  if (q.order !== DEFAULT_FEED_QUERY.order) sp.set("order", q.order);
  return sp;
}

/** statusSelectValue answers the one value the Status select shows. A result wins over a status. */
export function statusSelectValue(q: FeedQuery): string {
  if (q.result !== "") return q.result;
  return q.status;
}

const RUN_STATES = ["QUEUED", "RUNNING", "WAITING_INPUT"];

/**
 * applyStatusSelect maps one Status select choice to the query. A run state
 * sets status. A result sets result. The empty choice clears both.
 */
export function applyStatusSelect(q: FeedQuery, v: string): FeedQuery {
  if (v === "") return { ...q, status: "", result: "" };
  if (RUN_STATES.includes(v)) return { ...q, status: v, result: "" };
  return { ...q, status: "", result: v };
}

export function feedQueryIsDefault(q: FeedQuery): boolean {
  return writeFeedQuery(q).toString() === "";
}

/**
 * useFeedQuery reads the feed view from the URL. A setter replaces the
 * history entry, so a filter change does not fill the Back button.
 */
export function useFeedQuery(): [FeedQuery, (next: FeedQuery) => void] {
  const [searchParams, setSearchParams] = useSearchParams();
  const query = useMemo(() => parseFeedQuery(searchParams), [searchParams]);
  const setQuery = useCallback(
    (next: FeedQuery) => setSearchParams(writeFeedQuery(next), { replace: true }),
    [setSearchParams],
  );
  return [query, setQuery];
}
