// The route context of the assistant. A session carries what the page
// shows, so the assistant knows the build, job, team, or controller in view.
import { matchPath } from "react-router-dom";
import type { AssistantSessionContext } from "../../types/assistant";

/** RouteHint adds what a route alone cannot tell, such as a cell. */
export interface RouteHint {
  cell?: boolean;
}

interface Row {
  pattern: string;
  route: string;
}

/** ROUTE_TABLE is matched in order. The first match wins. */
export const ROUTE_TABLE: readonly Row[] = [
  { pattern: "/pipelines/:team/:job/tests/:case", route: "test-case" },
  { pattern: "/pipelines/:team/:job/:run", route: "build" },
  { pattern: "/pipelines/:team/:job", route: "job" },
  { pattern: "/pipelines/:team", route: "team" },
  { pattern: "/controllers/:cluster/:namespace/:name", route: "controller" },
  { pattern: "/cells", route: "cells" },
  { pattern: "/locks", route: "locks" },
  { pattern: "/activity", route: "activity" },
  { pattern: "/", route: "dashboard" },
];

/** routeContext maps a location onto the session context of the route. */
export function routeContext(pathname: string, hint: RouteHint = {}): AssistantSessionContext {
  for (const row of ROUTE_TABLE) {
    const m = matchPath({ path: row.pattern, end: true }, pathname);
    if (!m) continue;
    const p = m.params as Record<string, string>;
    const dec = (v: string | undefined) => (v === undefined ? "" : decodeURIComponent(v));
    switch (row.route) {
      case "test-case":
        return { route: row.route, team: dec(p.team), job: dec(p.job) };
      case "build":
        return { route: row.route, team: dec(p.team), job: dec(p.job), run: dec(p.run) };
      case "job":
        return { route: row.route, team: dec(p.team), job: dec(p.job) };
      case "team":
        return { route: row.route, team: dec(p.team) };
      case "controller": {
        const ref = `${dec(p.cluster)}/${dec(p.namespace)}/${dec(p.name)}`;
        return hint.cell ? { route: row.route, controller: ref, cell: ref } : { route: row.route, controller: ref };
      }
      default:
        return { route: row.route };
    }
  }
  // Any other path answers its first segment.
  const first = pathname.split("/").filter(Boolean)[0] ?? "";
  return { route: first || "dashboard" };
}

const KEYS = ["team", "job", "run", "controller", "cell"] as const;

/** contextDiffers reports whether two contexts name another object. */
export function contextDiffers(a: AssistantSessionContext, b: AssistantSessionContext): boolean {
  return KEYS.some((k) => (a[k] ?? "") !== (b[k] ?? ""));
}

/** contextLabel names the object of a context for the drawer chip. */
export function contextLabel(ctx: AssistantSessionContext): string {
  switch (ctx.route) {
    case "build":
      return `Build ${(ctx.run ?? "").slice(0, 8)} of ${ctx.team ?? ""}/${ctx.job ?? ""}`;
    case "job":
    case "test-case":
      return `Job ${ctx.team ?? ""}/${ctx.job ?? ""}`;
    case "team":
      return `Team ${ctx.team ?? ""}`;
    case "controller":
      return ctx.cell ? `Cell ${ctx.cell}` : `Controller ${ctx.controller ?? ""}`;
    case "cells":
      return "Cells";
    case "locks":
      return "Locks";
    case "activity":
      return "Activity";
    case "dashboard":
      return "Dashboard";
    default:
      return ctx.route ?? "";
  }
}
