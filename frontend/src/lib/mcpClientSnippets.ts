// The MCP client configuration blocks shown by the dashboard connect tile.
// Each block copies a block under "## Configure a client" in
// docs/agents/connecting.md. The parity test in ConnectClientTile.test.tsx
// fails when a block drifts from the handbook.

export type McpClientId = "cursor" | "vscode" | "stdio";

export interface McpClientSnippet {
  id: McpClientId;
  label: string;
  code: string;
}

/** The address the handbook blocks use; the tile replaces it with the real URL. */
export const MCP_URL_PLACEHOLDER = "https://<dashboard-host>/api/v1/mcp";

// Single-quoted lines keep ${env:...} and ${input:...} from interpolating.
export const MCP_CLIENT_SNIPPETS: McpClientSnippet[] = [
  {
    id: "cursor",
    label: "Cursor",
    code: [
      "{",
      '  "mcpServers": {',
      '    "varroa": {',
      `      "url": "${MCP_URL_PLACEHOLDER}",`,
      '      "headers": {',
      '        "Authorization": "Bearer ${env:VARROA_API_KEY}"',
      "      }",
      "    }",
      "  }",
      "}",
    ].join("\n"),
  },
  {
    id: "vscode",
    label: "GitHub Copilot",
    code: [
      "{",
      '  "inputs": [',
      "    {",
      '      "type": "promptString",',
      '      "id": "varroa-api-key",',
      '      "description": "Varroa API key",',
      '      "password": true',
      "    }",
      "  ],",
      '  "servers": {',
      '    "varroa": {',
      '      "type": "http",',
      `      "url": "${MCP_URL_PLACEHOLDER}",`,
      '      "headers": {',
      '        "Authorization": "Bearer ${input:varroa-api-key}"',
      "      }",
      "    }",
      "  }",
      "}",
    ].join("\n"),
  },
  {
    id: "stdio",
    label: "Stdio bridge",
    code: [
      "{",
      '  "mcpServers": {',
      '    "varroa": {',
      '      "command": "varroactl",',
      '      "args": ["mcp"]',
      "    }",
      "  }",
      "}",
    ].join("\n"),
  },
];

/** Replaces the handbook address in a snippet with the dashboard's own URL. */
export function withMcpUrl(code: string, url: string): string {
  return code.split(MCP_URL_PLACEHOLDER).join(url);
}
