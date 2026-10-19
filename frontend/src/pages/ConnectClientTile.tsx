import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { BFF_BASE, getMyAssistant, listApiKeys } from "../api/client";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import { Tabs, tabPanelProps } from "../components/Tabs";
import { MCP_CLIENT_SNIPPETS, withMcpUrl } from "../lib/mcpClientSnippets";
import styles from "./ConnectClientTile.module.css";

const PANEL_ID = "connect-client";

/** The absolute MCP endpoint of this dashboard. */
export function mcpUrl(): string {
  return new URL(`${BFF_BASE}/mcp`, window.location.origin).href;
}

/**
 * Shows how to connect an MCP client to a user who has no API key and has not
 * turned on the assistant. The card never shows or holds a key.
 */
export function ConnectClientTile() {
  const keys = useQuery({ queryKey: ["apikeys"], queryFn: listApiKeys });
  const assistant = useQuery({ queryKey: ["me-assistant"], queryFn: getMyAssistant, retry: false });
  const [tab, setTab] = useState<string>(MCP_CLIENT_SNIPPETS[0].id);
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  const assistantOn = !!(assistant.data?.available && assistant.data?.enabled);
  if (!keys.isSuccess || keys.data.items.length > 0 || assistant.isPending || assistantOn) {
    return null;
  }

  const url = mcpUrl();
  const current = MCP_CLIENT_SNIPPETS.find((s) => s.id === tab) ?? MCP_CLIENT_SNIPPETS[0];

  async function copy() {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
    } catch {
      // The address stays selectable on screen when the clipboard is blocked.
    }
  }

  return (
    <div className={styles.tile} data-testid="connect-client-tile">
      <Card title="Connect an MCP client">
        <div className={styles.urlRow}>
          <code className={styles.url} aria-label="MCP URL">
            {url}
          </code>
          <Button size="sm" onClick={copy}>
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>
        <Link to="/api-keys" className={styles.keyLink}>
          Create an API key
        </Link>
        <Tabs
          tabs={MCP_CLIENT_SNIPPETS.map(({ id, label }) => ({ id, label }))}
          activeTab={current.id}
          onSelect={setTab}
          idBase={PANEL_ID}
          ariaLabel="MCP client"
        />
        <pre className={styles.code} {...tabPanelProps(PANEL_ID, current.id)}>
          {withMcpUrl(current.code, url)}
        </pre>
      </Card>
    </div>
  );
}
