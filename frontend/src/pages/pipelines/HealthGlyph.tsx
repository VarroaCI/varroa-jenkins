import { Cloud, CloudLightning, CloudRain, CloudSun, Sun, type LucideIcon } from "lucide-react";
import type { JobHealth, PipelineHealthBucket } from "../../types";
import styles from "./HealthGlyph.module.css";

type ShownBucket = Exclude<PipelineHealthBucket, "unknown">;

const BUCKET_GLYPH: Record<ShownBucket, { Icon: LucideIcon; name: string; cls: string }> = {
  sunny: { Icon: Sun, name: "Sunny", cls: styles.sunny },
  partlyCloudy: { Icon: CloudSun, name: "Partly cloudy", cls: styles.partlyCloudy },
  cloudy: { Icon: Cloud, name: "Cloudy", cls: styles.cloudy },
  rainy: { Icon: CloudRain, name: "Rainy", cls: styles.rainy },
  stormy: { Icon: CloudLightning, name: "Stormy", cls: styles.stormy },
};

/** healthLabel names a job's bucket and its pass count, so a screen reader
 * announces the state and the numbers behind it. */
export function healthLabel(health: JobHealth): string {
  if (health.bucket === "unknown") return "No counted build yet";
  const { name } = BUCKET_GLYPH[health.bucket];
  return `${name}: ${health.passed} of ${health.counted} counted builds passed`;
}

/**
 * HealthGlyph renders the weather glyph of a job's health. It names its own
 * state, so a screen reader announces the state rather than the image. A
 * job with no counted run renders no glyph.
 */
export function HealthGlyph({ health }: { health: JobHealth }) {
  if (health.bucket === "unknown") return null;
  const { Icon, cls } = BUCKET_GLYPH[health.bucket];
  const label = healthLabel(health);
  return (
    <span className={`${styles.healthCell} ${cls}`} role="img" aria-label={label} title={label}>
      <Icon aria-hidden="true" size={18} />
    </span>
  );
}
