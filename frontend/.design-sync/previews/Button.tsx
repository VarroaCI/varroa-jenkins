import { Button } from "varroa-frontend";

export function Variants() {
  return (
    <div style={{ display: "flex", gap: 12, alignItems: "center", flexWrap: "wrap" }}>
      <Button variant="primary">Provision controller</Button>
      <Button variant="default">Apply bundle</Button>
      <Button variant="ghost">Cancel</Button>
    </div>
  );
}

export function Sizes() {
  return (
    <div style={{ display: "flex", gap: 12, alignItems: "center" }}>
      <Button variant="primary">Default size</Button>
      <Button variant="primary" size="sm">Small</Button>
    </div>
  );
}

export function Disabled() {
  return (
    <div style={{ display: "flex", gap: 12, alignItems: "center" }}>
      <Button variant="primary" disabled>Provisioning…</Button>
      <Button variant="default" disabled>Apply bundle</Button>
    </div>
  );
}
