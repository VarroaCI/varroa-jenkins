import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import fs from "fs";
import path from "path";
import { renderWithProviders } from "../test/render-utils";
import { ConnectClientTile, mcpUrl } from "./ConnectClientTile";
import { MCP_CLIENT_SNIPPETS, MCP_URL_PLACEHOLDER } from "../lib/mcpClientSnippets";

const mockListKeys = vi.fn();
const mockGetAssistant = vi.fn();
vi.mock("../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/client")>()),
  listApiKeys: () => mockListKeys(),
  getMyAssistant: () => mockGetAssistant(),
}));

const KEYS_NONE = { items: [] };
const ASSISTANT_OFF = { available: false };

function renderTile() {
  return renderWithProviders(<ConnectClientTile />);
}

/** Reads the first fenced block under each ### heading of "## Configure a client". */
function handbookBlocks(): { label: string; code: string }[] {
  const file = path.resolve(__dirname, "../../../docs/agents/connecting.md");
  const lines = fs.readFileSync(file, "utf8").split("\n");
  const start = lines.indexOf("## Configure a client");
  let end = lines.length;
  for (let i = start + 1; i < lines.length; i++) {
    if (lines[i].startsWith("## ")) {
      end = i;
      break;
    }
  }
  const out: { label: string; code: string }[] = [];
  for (let i = start + 1; i < end; i++) {
    if (!lines[i].startsWith("### ")) continue;
    const label = lines[i].slice(4);
    let open = i + 1;
    while (!lines[open].startsWith("```")) open++;
    let close = open + 1;
    while (lines[close] !== "```") close++;
    out.push({ label, code: lines.slice(open + 1, close).join("\n") });
  }
  return out;
}

describe("ConnectClientTile", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockListKeys.mockResolvedValue(KEYS_NONE);
    mockGetAssistant.mockResolvedValue(ASSISTANT_OFF);
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
  });

  it("shows the card with no key and no active assistant", async () => {
    renderTile();
    expect(await screen.findByText("Connect an MCP client")).toBeInTheDocument();
  });

  it("hides the card when a key exists", async () => {
    mockListKeys.mockResolvedValue({ items: [{ prefix: "vk_abc" }] });
    renderTile();
    await waitFor(() => expect(mockListKeys).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByTestId("connect-client-tile")).not.toBeInTheDocument());
  });

  it("hides the card when the assistant is available and enabled", async () => {
    mockGetAssistant.mockResolvedValue({ available: true, enabled: true });
    renderTile();
    await waitFor(() => expect(mockGetAssistant).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByTestId("connect-client-tile")).not.toBeInTheDocument());
  });

  it("hides the card when the key read fails", async () => {
    mockListKeys.mockRejectedValue(new Error("down"));
    renderTile();
    await waitFor(() => expect(mockListKeys).toHaveBeenCalled());
    expect(screen.queryByTestId("connect-client-tile")).not.toBeInTheDocument();
  });

  it("hides the card while the key read loads", () => {
    mockListKeys.mockReturnValue(new Promise(() => {}));
    renderTile();
    expect(screen.queryByTestId("connect-client-tile")).not.toBeInTheDocument();
  });

  it("shows the card when the assistant read fails and no key exists", async () => {
    mockGetAssistant.mockRejectedValue(new Error("no assistant"));
    renderTile();
    expect(await screen.findByText("Connect an MCP client")).toBeInTheDocument();
  });

  it("copies the MCP URL and reads Copied", async () => {
    renderTile();
    await screen.findByText("Connect an MCP client");
    await userEvent.click(screen.getByRole("button", { name: "Copy" }));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith(mcpUrl());
    expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument();
  });

  it("links Create an API key to /api-keys", async () => {
    renderTile();
    const link = await screen.findByRole("link", { name: "Create an API key" });
    expect(link).toHaveAttribute("href", "/api-keys");
  });

  it("shows the real URL in every panel and no placeholder", async () => {
    renderTile();
    await screen.findByText("Connect an MCP client");
    for (const snippet of MCP_CLIENT_SNIPPETS) {
      await userEvent.click(screen.getByRole("tab", { name: snippet.label }));
      const panel = screen.getByRole("tabpanel");
      const text = panel.textContent ?? "";
      expect(text).not.toContain(MCP_URL_PLACEHOLDER);
      if (snippet.id !== "stdio") expect(text).toContain(mcpUrl());
    }
    expect(within(screen.getByTestId("connect-client-tile")).getByLabelText("MCP URL")).toHaveTextContent(mcpUrl());
  });

  it("each snippet equals its block in connecting.md", () => {
    const blocks = handbookBlocks();
    expect(blocks.map((b) => b.label)).toEqual(MCP_CLIENT_SNIPPETS.map((s) => s.label));
    MCP_CLIENT_SNIPPETS.forEach((snippet, i) => {
      expect(snippet.code).toBe(blocks[i].code);
    });
  });
});
