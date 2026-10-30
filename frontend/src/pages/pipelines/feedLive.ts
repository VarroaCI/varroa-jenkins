import type { FleetRunsParams } from "../../api/client";
import type { FleetRunRow } from "../../types";
import type { FeedQuery } from "./feedQuery";

/** feedParams turns the view into the query of one page read. The cursor is added per page. */
export function feedParams(q: FeedQuery): FleetRunsParams {
  return {
    tenant: q.tenant,
    job: q.job,
    trigger: q.trigger,
    status: q.status,
    result: q.result,
    branch: q.branch,
    actor: q.actor,
    since: q.since,
    q: q.q,
    sort: q.sort,
    order: q.order,
  };
}

/** isDefaultSort is true for the started sort in descending order. Only this sort takes live rows in place. */
export function isDefaultSort(q: FeedQuery): boolean {
  return q.sort === "started" && q.order === "desc";
}

/**
 * hasStateFilter is true when the view filters on a run state or a result.
 * Only these two change while a run is live, so only they can leave a row
 * stale in a view. A live frame refetches such a view.
 */
export function hasStateFilter(q: FeedQuery): boolean {
  return q.status !== "" || q.result !== "";
}

/**
 * mergeFirstPage applies a fresh first page to the loaded pages. A loaded
 * row with the same run id takes the fresh value where it stands, on any
 * page. A fresh row that no loaded page holds goes to the front of the first
 * page, in fresh order. Older pages keep their rows.
 */
export function mergeFirstPage(pages: FleetRunRow[][], fresh: FleetRunRow[]): FleetRunRow[][] {
  if (pages.length === 0) return [fresh];
  const freshById = new Map(fresh.map((r) => [r.runId, r]));
  const known = new Set<string>();
  const replaced = pages.map((page) =>
    page.map((r) => {
      known.add(r.runId);
      return freshById.get(r.runId) ?? r;
    }),
  );
  const absent = fresh.filter((r) => !known.has(r.runId));
  return [[...absent, ...replaced[0]], ...replaced.slice(1)];
}
