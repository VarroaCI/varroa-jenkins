import { Console } from "varroa-frontend";

export function BuildLog() {
  return (
    <div style={{ maxWidth: 640 }}>
      <Console
        maxHeight={240}
        lines={[
          { timestamp: "10:42:01", level: "INFO", source: "operator", message: "Reconciling controller smoke-main" },
          { timestamp: "10:42:02", level: "OK", source: "gateway", message: "mite registered, mTLS cert issued" },
          { timestamp: "10:42:03", level: "INFO", source: "mite", message: "Applying composed bundle platform-base@a1b2c3d" },
          { timestamp: "10:42:05", level: "WARN", source: "jenkins", message: "Plugin role-strategy requires restart" },
          { timestamp: "10:42:09", level: "ERROR", source: "jenkins", message: "JCasC preflight failed: unknown plugin 'foo'" },
          { timestamp: "10:42:12", level: "DEBUG", source: "operator", message: "Requeue after 5s" },
        ]}
      />
    </div>
  );
}
