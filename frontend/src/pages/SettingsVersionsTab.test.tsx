import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("../api/client", () => ({
  getVersionProfiles: vi.fn(),
  listClusters: vi.fn(() => Promise.resolve([{ name: "core", core: true }])),
}));

import { getVersionProfiles } from "../api/client";
import SettingsVersionsTab from "./SettingsVersionsTab";
import type { VersionProfileDetail } from "../types";

const mockGet = getVersionProfiles as ReturnType<typeof vi.fn>;

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <SettingsVersionsTab />
    </QueryClientProvider>,
  );
}

const readyLts: VersionProfileDetail = {
  name: "jenkins-version-2-555",
  version: "2.555",
  channel: "lts",
  recommended: true,
  pluginSetRef: "jenkins-version-2-555-pluginset",
  contentRef: "jenkins-version-2-555-pluginset-content",
  pluginCount: 71,
  hasJcasc: true,
  conditions: [
    { type: "PluginSetReady", status: "True", message: "materialized ok" },
    { type: "LockJcascMismatch", status: "False" },
  ],
};

const unreadyWeekly: VersionProfileDetail = {
  name: "jenkins-version-2-570",
  version: "2.570",
  channel: "weekly",
  pluginSetRef: "jenkins-version-2-570-pluginset",
  pluginCount: 0,
  hasJcasc: false,
  conditions: [
    { type: "PluginSetReady", status: "False", reason: "SourceUnavailable", message: "source cm missing" },
  ],
};

const metadataOnly: VersionProfileDetail = {
  name: "jenkins-version-meta",
  version: "2.541",
  channel: "lts",
  hasJcasc: false,
  conditions: [],
};

beforeEach(() => {
  mockGet.mockReset();
});

describe("SettingsVersionsTab", () => {
  it("renders ready, unready (SourceUnavailable), and metadata-only rows", async () => {
    mockGet.mockResolvedValue([readyLts, unreadyWeekly, metadataOnly]);
    renderTab();

    // Ready status from the ready lts profile.
    await screen.findByText("Ready");
    // recommended badge.
    expect(screen.getByText("recommended")).toBeInTheDocument();
    // plugin count passthrough.
    expect(screen.getByText("71")).toBeInTheDocument();
    // SourceUnavailable reason text on the unready profile's Error cell.
    expect(screen.getByText(/SourceUnavailable/)).toBeInTheDocument();
    // Metadata-only row → em-dashes present.
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(1);
  });

  it("shows a warning EOL badge and a lock-mismatch chip with tooltip", async () => {
    const eolMismatch: VersionProfileDetail = {
      name: "jenkins-version-old",
      version: "2.541",
      channel: "lts",
      eol: "2020-01-01",
      pluginSetRef: "x",
      contentRef: "x-content",
      pluginCount: 10,
      hasJcasc: true,
      conditions: [
        { type: "PluginSetReady", status: "True" },
        { type: "LockJcascMismatch", status: "True", message: "overlay needs role-strategy" },
      ],
    };
    mockGet.mockResolvedValue([eolMismatch]);
    renderTab();

    await screen.findByText(/EOL 2020-01-01/);
    const chip = await screen.findByText("lock mismatch");
    expect(chip).toHaveAttribute("title", "overlay needs role-strategy");
  });

  it("shows the empty state when no profiles are installed", async () => {
    mockGet.mockResolvedValue([]);
    renderTab();
    await screen.findByText("No version profiles installed");
  });

  it("shows an error state when the fetch rejects", async () => {
    mockGet.mockRejectedValue(new Error("boom"));
    renderTab();
    await screen.findByText(/Error: boom/);
  });
});
