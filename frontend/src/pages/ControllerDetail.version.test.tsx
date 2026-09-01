import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastProvider } from "../components/Toast";
import type { ControllerDetail as ControllerDetailData } from "../hooks/useControllers";
import type { VersionCatalogEntry, VersionProfileDetail } from "../types";

// Mock the client module: real parsePreflightChecks, controllable
// updateController + getProvisioningConfig + getVersionProfiles.
const mockUpdateController = vi.fn();
// getVersionProfiles defaults to no profiles → line-item diff unavailable
// (count-delta + readiness fallback). Individual tests override it.
const mockGetVersionProfiles = vi.fn(() => Promise.resolve([] as VersionProfileDetail[]));
const catalog: VersionCatalogEntry[] = [
  { version: "2.570", channel: "weekly", name: "2.570", pluginSetReady: true, pluginCount: 40 },
  { version: "2.555", channel: "lts", recommended: true, name: "2.555", pluginSetReady: true, pluginCount: 50 },
  { version: "2.541", channel: "lts", eol: "2026-04-15", name: "2.541", pluginSetReady: false, pluginCount: 45 },
];
vi.mock("../api/client", async () => {
  const actual = await vi.importActual<typeof import("../api/client")>("../api/client");
  return {
    listClusters: vi.fn(),
    updateController: (...args: unknown[]) => mockUpdateController(...args),
    ControllerConflictError: actual.ControllerConflictError,
    getProvisioningConfig: vi.fn(() => Promise.resolve({ versions: catalog })),
    getVersionProfiles: () => mockGetVersionProfiles(),
    parsePreflightChecks: actual.parsePreflightChecks,
  };
});

import { VersionCard, VersionRollBanner } from "./ControllerDetail";

function makeCtrl(overrides: Partial<ControllerDetailData>): ControllerDetailData {
  return {
    name: "ci",
    namespace: "team-a",
    cluster: "core",
    phase: "Connected",
    endpoint: "https://ci.example.com",
    version: "2.541",
    miteConnected: true,
    ...overrides,
  } as ControllerDetailData;
}

function renderCard(ctrl: ControllerDetailData) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <VersionCard ctrl={ctrl} canUpdate={true} />
      </ToastProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  mockUpdateController.mockReset();
  mockUpdateController.mockResolvedValue({});
  mockGetVersionProfiles.mockReset();
  mockGetVersionProfiles.mockResolvedValue([]);
});

function makeProfile(version: string, plugins?: string[]): VersionProfileDetail {
  return {
    name: version,
    version,
    channel: "lts",
    hasJcasc: false,
    conditions: [],
    ...(plugins ? { plugins } : {}),
  } as VersionProfileDetail;
}

describe("VersionCard", () => {
  it("shows the upgrade chip and EOL badge for the desired version", async () => {
    renderCard(makeCtrl({ version: "2.541" }));
    // 2.541 is EOL and 2.555 is a newer recommended → upgrade available.
    expect(await screen.findByText(/Upgrade available → 2.555/)).toBeInTheDocument();
    // EOL appears on both the card badge and the picker option sub-text.
    expect(screen.getAllByText(/EOL 2026-04-15/).length).toBeGreaterThanOrEqual(1);
  });

  it("disables the picker with a note when a resourceOverlay pins the jenkins image", async () => {
    const overlayYaml = `
spec:
  template:
    spec:
      containers:
        - name: jenkins
          image: myrepo/jenkins:custom
`;
    renderCard(makeCtrl({ version: "2.555", resourceOverlay: { statefulSet: overlayYaml } }));
    expect(await screen.findByText(/pinned by a resourceOverlay/)).toBeInTheDocument();
    expect(screen.getByText("myrepo/jenkins:custom")).toBeInTheDocument();
    // Apply is disabled while inert.
    expect(screen.getByRole("button", { name: /Apply version/ })).toBeDisabled();
  });

  it("shows the plugin-count delta when a different target is chosen", async () => {
    renderCard(makeCtrl({ version: "2.541" }));
    // Select 2.555 (50 plugins) from current 2.541 (45).
    fireEvent.click(await screen.findByText("2.555"));
    expect(await screen.findByText(/Pinned plugins: 45 → 50 \(Δ\+5\)/)).toBeInTheDocument();
  });

  it("warns when the target plugin set is not materialized", async () => {
    renderCard(makeCtrl({ version: "2.555" }));
    fireEvent.click(await screen.findByText("2.541")); // pluginSetReady:false
    expect(await screen.findByText(/target plugin set not yet materialized/)).toBeInTheDocument();
  });

  it("renders inline preflight-fail checks from a 400 and does not throw", async () => {
    mockUpdateController.mockRejectedValue(
      new Error(
        '400 Bad Request: {"error":"preflight failed","checks":[{"id":"pluginCoreCompat","status":"fail","message":"core older than plugin baseline"}]}',
      ),
    );
    renderCard(makeCtrl({ version: "2.541" }));
    fireEvent.click(await screen.findByText("2.555"));
    fireEvent.click(screen.getByRole("button", { name: /Apply version/ }));
    expect(await screen.findByText(/core older than plugin baseline/)).toBeInTheDocument();
  });

  it("applies the selected version via updateController", async () => {
    renderCard(makeCtrl({ version: "2.541" }));
    fireEvent.click(await screen.findByText("2.555"));
    fireEvent.click(screen.getByRole("button", { name: /Apply version/ }));
    await waitFor(() =>
      expect(mockUpdateController).toHaveBeenCalledWith("core", "ci", "team-a", { spec: { version: "2.555" } }, { force: false }),
    );
  });

  it("renders the line-item plugin diff when both profiles expose plugins[]", async () => {
    mockGetVersionProfiles.mockResolvedValue([
      makeProfile("2.541", ["a@1.0", "role-strategy@742.vb", "old-plugin@1.0"]),
      makeProfile("2.555", ["a@1.0", "role-strategy@800.va", "new-plugin@2.0"]),
    ]);
    renderCard(makeCtrl({ version: "2.541" }));
    fireEvent.click(await screen.findByText("2.555"));
    // added / removed / version-changed all surface; count delta stays alongside.
    expect(await screen.findByText(/\+ new-plugin/)).toBeInTheDocument();
    expect(screen.getByText(/− old-plugin/)).toBeInTheDocument();
    expect(screen.getByText(/~ role-strategy: 742\.vb → 800\.va/)).toBeInTheDocument();
    expect(screen.getByText(/Pinned plugins: 45 → 50/)).toBeInTheDocument();
    // fallback note is gone when a real diff renders.
    expect(screen.queryByText(/per-plugin diff is not available/)).not.toBeInTheDocument();
  });

  it("matches an LTS line profile to a 3-segment version via the dotted-prefix rule", async () => {
    mockGetVersionProfiles.mockResolvedValue([
      makeProfile("2.541", ["a@1.0"]),
      makeProfile("2.555", ["a@1.0", "b@1.0"]),
    ]);
    // desired is the 3-segment patch of the 2.541 line profile.
    renderCard(makeCtrl({ version: "2.541.3" }));
    fireEvent.click(await screen.findByText("2.555"));
    expect(await screen.findByText(/\+ b/)).toBeInTheDocument();
    expect(screen.queryByText(/per-plugin diff is not available/)).not.toBeInTheDocument();
  });

  it("falls back to the count view when a profile lacks plugins[]", async () => {
    // target profile has no plugins[] → no line-item diff, keep count + note.
    mockGetVersionProfiles.mockResolvedValue([
      makeProfile("2.541", ["a@1.0"]),
      makeProfile("2.555"),
    ]);
    renderCard(makeCtrl({ version: "2.541" }));
    fireEvent.click(await screen.findByText("2.555"));
    expect(await screen.findByText(/Pinned plugins: 45 → 50/)).toBeInTheDocument();
    expect(screen.getByText(/A per-plugin diff is not available/)).toBeInTheDocument();
  });

  it("shows an in-progress chip when a roll is started", async () => {
    renderCard(
      makeCtrl({
        version: "2.555",
        versionStatus: { rollPending: true, rollReason: "VersionRollStarted" },
      }),
    );
    expect(await screen.findByText(/Upgrade in progress/)).toBeInTheDocument();
  });

  it("shows the pending-release chip and note when rollReason is UpgradePending", async () => {
    renderCard(
      makeCtrl({
        version: "2.555",
        versionStatus: { rollPending: true, rollReason: "UpgradePending", rollMessage: "held for 2.555.4" },
      }),
    );
    expect(await screen.findByText("Upgrade pending release")).toBeInTheDocument();
    // Rendered both beneath the chip (pendingMeta) and above the picker (VersionPicker's
    // own upgradePendingNote prop) — both surfaces show the same held-reason message.
    expect(screen.getAllByText("held for 2.555.4")).toHaveLength(2);
    // Not the same chip as the async in-progress state.
    expect(screen.queryByText(/Upgrade in progress/)).not.toBeInTheDocument();
  });

  it("does not show the pending-release chip or note when also upgradeBlocked", async () => {
    renderCard(
      makeCtrl({
        version: "2.555",
        versionStatus: {
          upgradeBlocked: true,
          blockedReason: "CoreOlderThanPluginBaseline",
          blockedMessage: "core too old",
          rollPending: true,
          rollReason: "UpgradePending",
          rollMessage: "held for 2.555.4",
        },
      }),
    );
    await screen.findByText("2.555");
    expect(screen.queryByText("Upgrade pending release")).not.toBeInTheDocument();
    expect(screen.queryByText("held for 2.555.4")).not.toBeInTheDocument();
  });
});

describe("VersionRollBanner", () => {
  it("renders a blocking banner when upgradeBlocked", () => {
    render(
      <VersionRollBanner
        versionStatus={{ upgradeBlocked: true, blockedReason: "CoreOlderThanPluginBaseline", blockedMessage: "core too old" }}
      />,
    );
    expect(screen.getByText(/Upgrade blocked: core too old/)).toBeInTheDocument();
  });

  it("renders a held banner when rollPending and not started", () => {
    render(
      <VersionRollBanner
        versionStatus={{ rollPending: true, rollReason: "VersionRollHeld", rollMessage: "gate held" }}
      />,
    );
    expect(screen.getByText(/Upgrade held: gate held/)).toBeInTheDocument();
  });

  it("blocked wins over held when both fire", () => {
    render(
      <VersionRollBanner
        versionStatus={{
          upgradeBlocked: true,
          blockedMessage: "blocked msg",
          rollPending: true,
          rollReason: "VersionRollHeld",
          rollMessage: "held msg",
        }}
      />,
    );
    expect(screen.getByText(/Upgrade blocked: blocked msg/)).toBeInTheDocument();
    expect(screen.queryByText(/Upgrade held/)).not.toBeInTheDocument();
  });

  it("renders nothing for in-progress (VersionRollStarted)", () => {
    const { container } = render(
      <VersionRollBanner versionStatus={{ rollPending: true, rollReason: "VersionRollStarted" }} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing for UpgradePending (surfaced via the VersionCard chip instead)", () => {
    const { container } = render(
      <VersionRollBanner versionStatus={{ rollPending: true, rollReason: "UpgradePending", rollMessage: "held for 2.555.4" }} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when versionStatus is absent", () => {
    const { container } = render(<VersionRollBanner versionStatus={undefined} />);
    expect(container).toBeEmptyDOMElement();
  });
});
