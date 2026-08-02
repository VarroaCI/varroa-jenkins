import { BundleHealthBadge } from "varroa-frontend";

export function Phases() {
  return (
    <div style={{ display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
      <BundleHealthBadge phase="Ready" />
      <BundleHealthBadge phase="Pending" />
      <BundleHealthBadge phase="Drifted" />
      <BundleHealthBadge phase="Invalid" />
    </div>
  );
}
