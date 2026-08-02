import { parse as parseYAML } from "yaml";

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
