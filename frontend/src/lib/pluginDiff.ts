// pluginDiff computes a line-item diff between two profiles' pinned plugin lists
// (the `plugins[]` field of GET /clusters/{cluster}/version-profiles: each entry is an
// "artifactId@version" line, or a bare id when unversioned).
//
// Semantics:
//   added   — ids present only in target
//   removed — ids present only in current
//   changed — present on BOTH sides with two KNOWN, differing versions. When
//             either side is unversioned (bare id) there is no version pair to
//             compare, so the plugin is treated as present-unchanged and never
//             surfaced as added/removed/changed.

export interface PluginChange {
  id: string;
  from: string;
  to: string;
}

export interface PluginDiff {
  added: string[];
  removed: string[];
  changed: PluginChange[];
}

// parsePin splits on the LAST "@": "group:art@1.2" → {id:"group:art", version:"1.2"};
// a bare "id" (or one with no "@" after position 0) → {id} with no version.
function parsePin(pin: string): { id: string; version?: string } {
  const at = pin.lastIndexOf("@");
  if (at <= 0) return { id: pin };
  return { id: pin.slice(0, at), version: pin.slice(at + 1) };
}

function toMap(pins: string[]): Map<string, string | undefined> {
  const m = new Map<string, string | undefined>();
  for (const p of pins) {
    const { id, version } = parsePin(p);
    m.set(id, version);
  }
  return m;
}

export function pluginDiff(current: string[], target: string[]): PluginDiff {
  const cur = toMap(current);
  const tgt = toMap(target);

  const added: string[] = [];
  const removed: string[] = [];
  const changed: PluginChange[] = [];

  for (const [id, tv] of tgt) {
    if (!cur.has(id)) {
      added.push(id);
      continue;
    }
    const cv = cur.get(id);
    // Only a change when both sides carry a known, differing version.
    if (cv !== undefined && tv !== undefined && cv !== tv) {
      changed.push({ id, from: cv, to: tv });
    }
  }
  for (const [id] of cur) {
    if (!tgt.has(id)) removed.push(id);
  }

  added.sort();
  removed.sort();
  changed.sort((a, b) => a.id.localeCompare(b.id));
  return { added, removed, changed };
}
