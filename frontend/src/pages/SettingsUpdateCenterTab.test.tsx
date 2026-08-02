import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import SettingsUpdateCenterTab from "./SettingsUpdateCenterTab";
import { renderWithProviders } from "../test/render-utils";
import { setupServer } from "msw/node";
import { http, HttpResponse } from "msw";

const BASE = "/api/v1";

const disabledStatus = { enabled: false, conditions: [], gaps: [], lastSyncTime: null, phase: "", pluginCount: 0, storeBytes: 0, storageType: "", pullThroughEnabled: false };
const enabledStatus = {
  enabled: true, phase: "Ready", pluginCount: 5, storeBytes: 2097152,
  conditions: [
    { type: "Available", status: "True", lastTransitionTime: "2025-01-01T00:00:00Z", reason: "Available", message: "update center is available" },
    { type: "Reconciling", status: "False", lastTransitionTime: "2025-01-02T00:00:00Z", reason: "Reconciled", message: "update center is reconciled" },
    { type: "Stale", status: "False", lastTransitionTime: "2025-01-03T00:00:00Z", reason: "Fresh", message: "inventory is fresh" },
    { type: "Paused", status: "False", lastTransitionTime: "2025-01-04T00:00:00Z", reason: "Active", message: "update center is active" },
  ],
  gaps: [
    { plugin: "blueocean", version: "1.25.3", requiredBy: "profile-a" },
    { plugin: "workflow-api", version: "2.47", requiredBy: "profile-b" },
  ],
  lastSyncTime: "2025-02-01T12:00:00Z",
  storageType: "oci",
  pullThroughEnabled: true,
};
const pluginsData = {
  enabled: true,
  plugins: [
    { name: "blueocean", version: "1.25.3", sha256: "abc123def456", sizeBytes: 1024 },
    { name: "workflow-api", version: "2.47", sha256: "def789ghi012", sizeBytes: 2048 },
    { name: "credentials", version: "2.0.1", sha256: "ghi345jkl678", sizeBytes: 512 },
  ],
};

let server: ReturnType<typeof setupServer>;

function createServer(statusHandler?: Parameters<typeof http.get>[1], pluginsHandler?: Parameters<typeof http.get>[1]) {
  const handlers = [
    http.get(`${BASE}/updatecenter`, statusHandler ?? (() => HttpResponse.json(disabledStatus))),
    http.get(`${BASE}/updatecenter/plugins`, pluginsHandler ?? (() => HttpResponse.json(pluginsData))),
  ];
  server = setupServer(...handlers);
  server.listen({ onUnhandledRequest: "bypass" });
}

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
});

afterEach(() => {
  vi.useRealTimers();
  server?.close();
});

describe("SettingsUpdateCenterTab", () => {
  it("shows loading spinner initially", () => {
    createServer(() => new HttpResponse(null, { status: 200, statusText: "OK" }));
    renderWithProviders(<SettingsUpdateCenterTab />);
    // LoadingSpinner renders a <div> with the spinner; just verify no error/disabled text
    expect(screen.queryByText("Update Center is not enabled on this cluster.")).not.toBeInTheDocument();
  });

  it("shows disabled message when status returns enabled:false", async () => {
    createServer();
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();
    expect(await screen.findByText("Update Center is not enabled on this cluster.")).toBeInTheDocument();
  });

  it("shows error banner when the status call fails", async () => {
    createServer(() => HttpResponse.json(null, { status: 500 }));
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();
    expect(await screen.findByText(/Error:/)).toBeInTheDocument();
  });

  it("shows error banner when the plugins call returns 502 after enabled status", async () => {
    createServer(
      () => HttpResponse.json(enabledStatus),
      () => HttpResponse.json({ error: "update center service unreachable" }, { status: 502 }),
    );
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();
    expect(await screen.findByText(/Error:/)).toBeInTheDocument();
  });

  it("renders the status card with all fields when enabled", async () => {
    createServer(
      () => HttpResponse.json(enabledStatus),
      () => HttpResponse.json(pluginsData),
    );
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();

    // Phase badge — await first to let async resolution complete in slow CI
    expect(await screen.findByText("Ready")).toBeInTheDocument();
    // Plugin count + store bytes
    expect(screen.getByText(/5 plugins/)).toBeInTheDocument();
    expect(screen.getByText(/2\.0 MB/)).toBeInTheDocument();

    // Conditions
    expect(screen.getByText("Available")).toBeInTheDocument();
    expect(screen.getByText("Reconciling")).toBeInTheDocument();
    expect(screen.getByText("Stale")).toBeInTheDocument();
    expect(screen.getByText("Paused")).toBeInTheDocument();

    // Metadata
    expect(screen.getByText(/Storage: oci/)).toBeInTheDocument();
    expect(screen.getByText(/Pull-through: enabled/)).toBeInTheDocument();
    expect(screen.getByText(/Last sync:/)).toBeInTheDocument();
    // lastSyncTime is set (not "never"), so verify the formatted date appears
    expect(screen.getByText(/2025/)).toBeInTheDocument();
  });

  it("shows 'never' when lastSyncTime is null", async () => {
    const statusNoSync = { ...enabledStatus, lastSyncTime: null };
    createServer(
      () => HttpResponse.json(statusNoSync),
      () => HttpResponse.json(pluginsData),
    );
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();
    expect(await screen.findByText(/Last sync: never/)).toBeInTheDocument();
  });

  it("renders gaps table when gaps.length > 0", async () => {
    createServer(
      () => HttpResponse.json(enabledStatus),
      () => HttpResponse.json(pluginsData),
    );
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();
    expect(await screen.findByText("Plugin gaps")).toBeInTheDocument();
    expect(screen.getAllByText("blueocean").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("workflow-api").length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText("profile-a")).toBeInTheDocument();
    expect(screen.getByText("profile-b")).toBeInTheDocument();
  });

  it("does not show gaps table when gaps.length === 0", async () => {
    const statusNoGaps = { ...enabledStatus, gaps: [] };
    createServer(
      () => HttpResponse.json(statusNoGaps),
      () => HttpResponse.json(pluginsData),
    );
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();

    // First await the status card to confirm DOM settled, then assert absence
    expect(await screen.findByText(/Storage:/)).toBeInTheDocument();
    expect(screen.queryByText("Plugin gaps")).not.toBeInTheDocument();
  });

  it("renders inventory plugins when enabled", async () => {
    createServer(
      () => HttpResponse.json(enabledStatus),
      () => HttpResponse.json(pluginsData),
    );
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();
    expect((await screen.findAllByText("blueocean")).length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("1.25.3").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("workflow-api").length).toBeGreaterThanOrEqual(1);
  });

  it("filters inventory when typing in the search box", async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    let lastQuery = "";
    createServer(
      () => HttpResponse.json(enabledStatus),
      ({ request }) => {
        const url = new URL(request.url);
        lastQuery = url.searchParams.get("q") || "";
        if (lastQuery === "blue") {
          return HttpResponse.json({ enabled: true, plugins: [{ name: "blueocean", version: "1.25.3", sha256: "abc", sizeBytes: 1024 }] });
        }
        return HttpResponse.json(pluginsData);
      },
    );
    renderWithProviders(<SettingsUpdateCenterTab />);
    await vi.runAllTimersAsync();

    // findBy, not getBy: runAllTimersAsync flushes pending timers but does not
    // guarantee React has committed the query result. On a loaded CI runner the
    // component is still showing its spinner at this point, so a synchronous
    // getBy fails while the same test passes locally.
    const input = await screen.findByPlaceholderText("Search plugins...");
    await user.type(input, "blue");
    // Advance past the 250ms debounce
    await vi.advanceTimersByTimeAsync(300);

    expect(lastQuery).toBe("blue");
  });
});
