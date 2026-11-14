import { parse as parseYAML } from "yaml";
import type { OverlayResourceKind } from "../types";

// overlayJenkinsImage returns the image declared for the container named
// "jenkins" in a statefulSet strategic-merge overlay YAML string, or null when
// absent or unparseable. Per change A, such an image makes spec.version inert.
// Unparseable input is treated as not-inert (the overlay editor owns the
// parse-error surface).
export function overlayJenkinsImage(statefulSetYaml?: string): string | null {
  if (!statefulSetYaml || !statefulSetYaml.trim()) return null;
  try {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const doc = parseYAML(statefulSetYaml) as any;
    const cs = doc?.spec?.template?.spec?.containers;
    if (!Array.isArray(cs)) return null;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const j = cs.find((c: any) => c?.name === "jenkins");
    return typeof j?.image === "string" ? j.image : null;
  } catch {
    return null;
  }
}

// The BFF preview handler starts each 400 reason with the request field that
// did not parse. The overlay editor names its fields with the short key.
const PREVIEW_FIELD_KEYS = new Map<string, OverlayResourceKind | "podOverrides">([
  ["resourceOverlay.statefulSet", "statefulSet"],
  ["resourceOverlay.service", "service"],
  ["resourceOverlay.ingress", "ingress"],
  ["podOverrides", "podOverrides"],
  ["compile podOverrides", "podOverrides"],
]);

// overlayFieldErrorFrom splits a preview 400 reason of the form
// "<field>: <reason>". It returns the editor field and the reason. It returns
// null when the prefix names no editor field.
export function overlayFieldErrorFrom(
  serverMessage: string,
): { field: OverlayResourceKind | "podOverrides"; message: string } | null {
  const sep = serverMessage.indexOf(": ");
  if (sep < 0) return null;
  const field = PREVIEW_FIELD_KEYS.get(serverMessage.slice(0, sep));
  const message = serverMessage.slice(sep + 2).trim();
  if (!field || !message) return null;
  return { field, message };
}
