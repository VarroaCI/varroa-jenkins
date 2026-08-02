import type { VersionCatalogEntry } from "../types";

export interface UpgradeInfo {
  managed: boolean; // desired version found in the catalog
  recommendedUpgrade?: string; // a recommended entry strictly newer than desired
  eol?: string; // desired entry's eol date (YYYY-MM-DD)
  eolPassed?: boolean; // eol <= today (UTC date compare)
}

// upgradeInfo derives upgrade/EOL indicators for a controller's desired version
// from the catalog. No version arithmetic — it relies on D's version-descending
// ordering guarantee plus date/string compares.
export function upgradeInfo(desired: string, versions: VersionCatalogEntry[]): UpgradeInfo {
  const i = versions.findIndex((v) => v.version === desired);
  if (i < 0) return { managed: false };
  const info: UpgradeInfo = { managed: true };
  // versions[] is version-descending: the first recommended entry is the newest.
  const r = versions.findIndex((v) => v.recommended === true);
  if (r >= 0 && r < i && versions[r].version !== desired) {
    info.recommendedUpgrade = versions[r].version;
  }
  const eol = versions[i].eol;
  if (eol) {
    info.eol = eol;
    info.eolPassed = eol <= new Date().toISOString().slice(0, 10);
  }
  return info;
}

// versionsDiffer reports true only when a and b differ AND neither is a dotted
// prefix of the other, so a 2-segment line ("2.552") vs a 3-segment running
// version ("2.552.1") is NOT treated as drift.
export function versionsDiffer(a?: string, b?: string): boolean {
  if (!a || !b || a === b) return false;
  return !(a.startsWith(b + ".") || b.startsWith(a + "."));
}
