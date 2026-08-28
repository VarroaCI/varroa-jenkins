import { Card, StatusPill, Button } from "varroa-frontend";

export function Basic() {
  return (
    <div style={{ maxWidth: 420 }}>
      <Card title="Controller details">
        <p style={{ color: "var(--text-2)", lineHeight: 1.5, margin: 0 }}>
          A surface for grouping related content. The header is optional and a
          right-aligned slot accepts actions or status.
        </p>
      </Card>
    </div>
  );
}

export function WithHeaderAction() {
  return (
    <div style={{ maxWidth: 420 }}>
      <Card
        title="smoke-main"
        headerRight={<StatusPill phase="Connected" size="sm" />}
      >
        <div style={{ display: "grid", gap: 8, color: "var(--text-2)" }}>
          <div>Namespace <strong style={{ color: "var(--text)" }}>jenkins</strong></div>
          <div>Replicas <strong style={{ color: "var(--text)" }}>1 / 1 ready</strong></div>
        </div>
      </Card>
    </div>
  );
}

export function WithFooterControls() {
  return (
    <div style={{ maxWidth: 420 }}>
      <Card title="Apply pending bundle">
        <p style={{ color: "var(--text-2)", margin: "0 0 16px" }}>
          A new composed bundle is ready for <strong style={{ color: "var(--text)" }}>smoke-main</strong>.
        </p>
        <div style={{ display: "flex", gap: 10 }}>
          <Button variant="primary" size="sm">Apply</Button>
          <Button variant="ghost" size="sm">Review diff</Button>
        </div>
      </Card>
    </div>
  );
}
