import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("../api/client", () => ({
  getVersionProfiles: vi.fn(),
  listClusters: vi.fn(() => Promise.resolve([{ name: "core", core: true }])),
  listVersionCandidates: vi.fn(() => Promise.resolve([])),
  promoteVersionCandidate: vi.fn(),
  getProvisioningDefaults: vi.fn(() => Promise.resolve({ spec: {} })),
}));

import { getVersionProfiles, listVersionCandidates, promoteVersionCandidate, getProvisioningDefaults } from "../api/client";
import SettingsVersionsTab from "./SettingsVersionsTab";
import type { VersionProfileDetail, VersionCandidate } from "../types";
import { ApiError } from "../hooks/useApi";

const mockGet = getVersionProfiles as ReturnType<typeof vi.fn>;
const mockListCandidates = listVersionCandidates as ReturnType<typeof vi.fn>;
const mockPromote = promoteVersionCandidate as ReturnType<typeof vi.fn>;
const mockGetDefaults = getProvisioningDefaults as ReturnType<typeof vi.fn>;

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
  mockListCandidates.mockReset();
  mockListCandidates.mockResolvedValue([]);
  mockPromote.mockReset();
  mockGetDefaults.mockReset();
  mockGetDefaults.mockResolvedValue({ spec: {} });
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

const readyCandidate: VersionCandidate = {
  metadata: { name: "jenkins-version-2-555-2-555-3" },
  spec: {
    profileRef: "jenkins-version-2-555",
    observedVersion: "2.555.2",
    resolveVersion: "2.555.3",
  },
  status: {
    phase: "Ready",
    conditions: [
      { type: "Resolved", status: "True" },
      { type: "ClosureClean", status: "True" },
      { type: "CoreCompatOK", status: "True" },
      { type: "PluginsServable", status: "True" },
      { type: "PreflightChecked", status: "True" },
      { type: "Promoted", status: "False" },
    ],
    preflight: {
      controllersChecked: 3,
      controllersFailing: 1,
      failingControllers: [
        { namespace: "team-a", name: "ci-1", conflictCount: 2, message: "plugin pin conflict: workflow-job" },
      ],
    },
  },
};

const pendingCandidate: VersionCandidate = {
  metadata: { name: "jenkins-version-2-555-2-555-4" },
  spec: {
    profileRef: "jenkins-version-2-555",
    observedVersion: "2.555.2",
    resolveVersion: "2.555.4",
  },
  status: {
    phase: "Pending",
    conditions: [
      { type: "Resolved", status: "True" },
      { type: "ClosureClean", status: "Unknown" },
      { type: "CoreCompatOK", status: "Unknown" },
      { type: "PluginsServable", status: "Unknown" },
      { type: "PreflightChecked", status: "False" },
      { type: "Promoted", status: "False" },
    ],
  },
};

const promotedCandidate: VersionCandidate = {
  metadata: { name: "jenkins-version-2-555-2-555-1" },
  spec: {
    profileRef: "jenkins-version-2-555",
    observedVersion: "2.555.0",
    resolveVersion: "2.555.1",
  },
  status: {
    phase: "Promoted",
    promotedAt: "2026-08-01T00:00:00Z",
  },
};

describe("SettingsVersionsTab upgrade candidates", () => {
  it("groups active vs. history candidates and renders all six condition chips", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValue([readyCandidate, promotedCandidate]);
    renderTab();

    await screen.findByText("Upgrade candidates");
    expect(screen.getByText("2.555.2 → 2.555.3")).toBeInTheDocument();
    for (const t of ["Resolved", "ClosureClean", "CoreCompatOK", "PluginsServable", "PreflightChecked", "Promoted"]) {
      expect(screen.getByText(t)).toBeInTheDocument();
    }
    // Failing-controllers rendering.
    expect(screen.getByText(/team-a\/ci-1/)).toBeInTheDocument();
    expect(screen.getByText(/plugin pin conflict: workflow-job/)).toBeInTheDocument();
    expect(screen.getByText(/1 of 3 controllers failing/)).toBeInTheDocument();

    // Promoted candidate goes to the collapsed history, not the active table.
    const history = screen.getByText("History (1)");
    expect(history).toBeInTheDocument();
  });

  it("disables Promote outside the Ready phase", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValue([readyCandidate, pendingCandidate]);
    renderTab();

    await screen.findByText("Upgrade candidates");
    const buttons = screen.getAllByRole("button", { name: "Promote" });
    expect(buttons).toHaveLength(2);
    expect(buttons.find((b) => !b.hasAttribute("disabled"))).toBeDefined();
    expect(buttons.filter((b) => b.hasAttribute("disabled"))).toHaveLength(1);
  });

  it("shows the auto-policy promote copy when upgradePolicy is unset", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValue([readyCandidate]);
    mockGetDefaults.mockResolvedValue({ spec: {} });
    renderTab();

    await screen.findByText("Upgrade candidates");
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    await screen.findByText(/In core, this rolls controllers pinned to line 2\.555 through the version-roll gate\./);
    expect(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Promote" })).toBeInTheDocument();
  });

  it("shows the manual-policy promote copy when upgradePolicy is explicitly manual", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValue([readyCandidate]);
    mockGetDefaults.mockResolvedValue({ spec: { upgradePolicy: "manual" } });
    renderTab();

    await screen.findByText("Upgrade candidates");
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    await screen.findByText(/In core, this holds controllers pinned to line 2\.555 with UpgradePending until released\./);
  });

  it("shows the auto-policy promote copy when upgradePolicy is explicitly auto", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValue([readyCandidate]);
    mockGetDefaults.mockResolvedValue({ spec: { upgradePolicy: "auto" } });
    renderTab();

    await screen.findByText("Upgrade candidates");
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    await screen.findByText(/In core, this rolls controllers pinned to line 2\.555 through the version-roll gate\./);
  });

  it("refetches the candidate list on successful promotion", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValueOnce([readyCandidate]).mockResolvedValueOnce([promotedCandidate]);
    mockPromote.mockResolvedValue({ ...readyCandidate, status: { ...readyCandidate.status, phase: "Promoted" } });
    renderTab();

    await screen.findByText("Upgrade candidates");
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    await screen.findByText(/In core, this rolls controllers/);
    await userEvent.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Promote" }));

    await waitFor(() => expect(mockListCandidates).toHaveBeenCalledTimes(2));
    await screen.findByText("No pending upgrade candidates.");
  });

  it("shows an inline error and does not refetch when promotion fails", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValue([readyCandidate]);
    mockPromote.mockRejectedValue(new Error("promote failed"));
    renderTab();

    await screen.findByText("Upgrade candidates");
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    await screen.findByText(/In core, this rolls controllers/);
    await userEvent.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Promote" }));

    await screen.findByText("promote failed");
    expect(mockListCandidates).toHaveBeenCalledTimes(1);
  });

  it("shows the cluster-mismatch empty state when candidates exist but none match this cluster's profiles", async () => {
    mockGet.mockResolvedValue([readyLts]);
    // readyCandidate.spec.profileRef ("jenkins-version-2-555") is not among readyLts's profile
    // names when the profile itself is named something the candidate doesn't reference.
    mockListCandidates.mockResolvedValue([{ ...readyCandidate, spec: { ...readyCandidate.spec, profileRef: "jenkins-version-9-999" } }]);
    renderTab();

    await screen.findByText("Upgrade candidates");
    expect(
      screen.getByText(
        "Upgrade candidates aren't tracked per cluster, and none of the current candidates match this cluster's version profiles.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("No pending upgrade candidates.")).not.toBeInTheDocument();
  });

  it("shows the generic empty state when there are genuinely no candidates", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates.mockResolvedValue([]);
    renderTab();

    await screen.findByText("Upgrade candidates");
    expect(screen.getByText("No pending upgrade candidates.")).toBeInTheDocument();
    expect(
      screen.queryByText(
        "Upgrade candidates aren't tracked per cluster, and none of the current candidates match this cluster's version profiles.",
      ),
    ).not.toBeInTheDocument();
  });

  it("shows the not-available message and refetches when promotion conflicts (409)", async () => {
    mockGet.mockResolvedValue([readyLts]);
    mockListCandidates
      .mockResolvedValueOnce([readyCandidate])
      .mockResolvedValueOnce([{ ...readyCandidate, status: { ...readyCandidate.status, phase: "Superseded" } }]);
    mockPromote.mockRejectedValue(new ApiError(409, "conflict"));
    renderTab();

    await screen.findByText("Upgrade candidates");
    await userEvent.click(screen.getByRole("button", { name: "Promote" }));
    await screen.findByText(/In core, this rolls controllers/);
    await userEvent.click(within(screen.getByRole("alertdialog")).getByRole("button", { name: "Promote" }));

    await screen.findByText("This candidate is no longer available for promotion — the list has been refreshed.");
    await waitFor(() => expect(mockListCandidates).toHaveBeenCalledTimes(2));
  });
});
