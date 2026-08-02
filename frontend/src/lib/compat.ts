import type { CatalogItemCompat, CatalogItemSummary, CatalogItemVerdict } from "../types";

/**
 * The reserved CatalogSource name. Items whose `sourceRef` is this were derived
 * from the update-center plugin store rather than fetched from a git or OCI
 * catalog.
 */
export const UPDATE_CENTER_SOURCE = "varroa-update-center";

/**
 * Verdict precedence, worst first. It mirrors the operator's ordering exactly:
 * core-too-old > dep-below-minimum > lock-too-old > unknown > compatible.
 */
const VERDICT_RANK: Record<CatalogItemVerdict, number> = {
  "core-too-old": 0,
  "dep-below-minimum": 1,
  "lock-too-old": 2,
  unknown: 3,
  compatible: 4,
};

/** Human-readable label for each verdict. */
export const VERDICT_LABEL: Record<CatalogItemVerdict, string> = {
  "core-too-old": "Needs newer Jenkins",
  "dep-below-minimum": "Dependency below minimum",
  "lock-too-old": "Lock too old",
  unknown: "Unknown",
  compatible: "Compatible",
};

/**
 * Whether a verdict warrants a visible warning. Advisory in every case: a badge
 * never disables selection, because derivability blocks and compatibility only
 * advises.
 */
export function isWarningVerdict(v: CatalogItemVerdict): boolean {
  return v === "core-too-old" || v === "dep-below-minimum" || v === "lock-too-old";
}

/** The worst verdict across profiles, or null when there is nothing to judge. */
export function worstVerdict(compat?: CatalogItemCompat[]): CatalogItemVerdict | null {
  if (!compat || compat.length === 0) return null;
  return compat.reduce<CatalogItemVerdict>(
    (worst, c) => (VERDICT_RANK[c.verdict] < VERDICT_RANK[worst] ? c.verdict : worst),
    "compatible",
  );
}

/** Groups a plugin's versions under one row. */
export interface PluginGroup {
  pluginName: string;
  /** Every stored version of this plugin, newest first by the comparator. */
  versions: CatalogItemSummary[];
}

/**
 * Orders two Jenkins plugin versions.
 *
 * This is a port of the ordering rules the operator applies, restricted to what
 * a version selector needs: numeric runs compare numerically and everything
 * else compares as text, so `1413.v2ff1a_5e720fa_` sorts above
 * `1384.vdc05a_48f535f` rather than lexically below it. It is deliberately NOT
 * used for any decision — the operator resolves and pins; this only decides
 * which entry a collapsed row shows first.
 */
export function comparePluginVersions(a: string, b: string): number {
  const at = a.toLowerCase().split(/[._-]/);
  const bt = b.toLowerCase().split(/[._-]/);
  for (let i = 0; i < Math.max(at.length, bt.length); i++) {
    const x = at[i] ?? "";
    const y = bt[i] ?? "";
    if (x === y) continue;
    const xn = /^\d+$/.test(x);
    const yn = /^\d+$/.test(y);
    if (xn && yn) return Number(x) - Number(y);
    // A missing component is treated as zero, so "1.0" equals "1".
    if (x === "") return yn && Number(y) === 0 ? 0 : -1;
    if (y === "") return xn && Number(x) === 0 ? 0 : 1;
    // A numeric run outranks a qualifier.
    if (xn !== yn) return xn ? 1 : -1;
    return x < y ? -1 : 1;
  }
  return 0;
}

/**
 * Collapses update-center items by plugin, so a plugin stored at three versions
 * is one row with a version selector rather than three rows.
 *
 * The default version is the one an eligible profile lock pins, when exactly one
 * lock mentions the plugin; otherwise the highest version by the comparator.
 * With a single stored version — the common case — both rules agree.
 */
export function groupByPlugin(
  items: CatalogItemSummary[],
  lockPinsByPlugin?: Record<string, string[]>,
): PluginGroup[] {
  const byName = new Map<string, CatalogItemSummary[]>();
  for (const item of items) {
    const key = item.pluginName || item.name;
    const list = byName.get(key);
    if (list) list.push(item);
    else byName.set(key, [item]);
  }

  const groups: PluginGroup[] = [];
  for (const [pluginName, versions] of byName) {
    const sorted = [...versions].sort((a, b) =>
      comparePluginVersions(b.version ?? "", a.version ?? ""),
    );
    const pins = lockPinsByPlugin?.[pluginName];
    if (pins && pins.length === 1) {
      const pinnedIdx = sorted.findIndex((v) => v.version === pins[0]);
      if (pinnedIdx > 0) {
        const [pinned] = sorted.splice(pinnedIdx, 1);
        sorted.unshift(pinned);
      }
    }
    groups.push({ pluginName, versions: sorted });
  }
  groups.sort((a, b) => a.pluginName.localeCompare(b.pluginName));
  return groups;
}
