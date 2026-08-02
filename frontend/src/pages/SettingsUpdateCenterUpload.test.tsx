import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { setupServer } from "msw/node";
import { http, HttpResponse } from "msw";
import { renderWithProviders } from "../test/render-utils";

const BASE = "/api/v1";

// The upload section is gated on the updatecenter/upload verb; the mock is
// switched per test through this ref.
const perms = { canUpload: true };

vi.mock("../hooks/usePermissions", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../hooks/usePermissions")>();
  return {
    ...actual,
    canDoGlobal: (_p: unknown, resource: string, verb: string) =>
      resource === "updatecenter" && verb === "upload" ? perms.canUpload : true,
  };
});

// Imported after the mock so the component picks it up.
const { default: SettingsUpdateCenterTab } = await import("./SettingsUpdateCenterTab");

const enabledStatus = {
  enabled: true, phase: "Ready", pluginCount: 1, storeBytes: 1024,
  conditions: [], gaps: [], lastSyncTime: null, storageType: "local", pullThroughEnabled: true,
};

const uploadResult = {
  plugin: { name: "varroa-mcp-tools", version: "1.0.0", sha256: "sha256:abc", requiredCore: "2.492" },
  dryRun: true,
  closure: [
    { name: "workflow-api", min: "1384.vdc05a_48f535f", status: "satisfied-store", resolvedVersion: "1413.v2ff1a_5e720fa_", source: "store" },
    { name: "some-lib", min: "1.2", status: "planned-fetch", resolvedVersion: "1.9", source: "upstream", fetched: true },
  ],
  optionalDependencies: [{ name: "junit", min: "1.0" }],
  warnings: [{ code: "lock-too-old", plugin: "credentials", min: "1400.v0", message: "the declared version is older" }],
};

const rejectionBody = {
  error: "unresolved-dependencies",
  message: "1 of 2 mandatory dependencies could not be resolved",
  unresolved: [{
    name: "acme-internal", min: "2.0", reason: "not-in-store",
    foundInStore: null, foundDeclared: null, foundUpstream: null,
    remediation: "pull-through is disabled; seed this plugin via spec.seed.refs",
  }],
};

let server: ReturnType<typeof setupServer>;
let uploadCalls: string[] = [];
let pluginsCalls = 0;

function startServer(uploadHandler: Parameters<typeof http.post>[1]) {
  uploadCalls = [];
  pluginsCalls = 0;
  server = setupServer(
    http.get(`${BASE}/updatecenter`, () => HttpResponse.json(enabledStatus)),
    http.get(`${BASE}/updatecenter/plugins`, () => {
      pluginsCalls++;
      return HttpResponse.json({ enabled: true, plugins: [] });
    }),
    http.post(`${BASE}/updatecenter/plugins`, uploadHandler),
  );
  server.listen({ onUnhandledRequest: "bypass" });
}

async function pickFile(user: ReturnType<typeof userEvent.setup>) {
  const input = screen.getByLabelText("Plugin artifact") as HTMLInputElement;
  await user.upload(input, new File(["hpi-bytes"], "varroa-mcp-tools.hpi", { type: "application/octet-stream" }));
}

beforeEach(() => {
  perms.canUpload = true;
});

afterEach(() => {
  server?.close();
});

describe("SettingsUpdateCenterTab — upload", () => {
  it("does not render the section without the upload verb", async () => {
    perms.canUpload = false;
    startServer(() => HttpResponse.json(uploadResult));
    renderWithProviders(<SettingsUpdateCenterTab />);

    await screen.findByText("Plugin inventory");
    expect(screen.queryByText("Upload plugin")).toBeNull();
    expect(screen.queryByLabelText("Plugin artifact")).toBeNull();
  });

  it("renders the section with the upload verb", async () => {
    startServer(() => HttpResponse.json(uploadResult));
    renderWithProviders(<SettingsUpdateCenterTab />);

    expect(await screen.findByText("Upload plugin")).toBeTruthy();
    expect(screen.getByLabelText("Plugin artifact")).toBeTruthy();
  });

  it("previews the closure with a dry run and stores nothing", async () => {
    startServer(({ request }) => {
      uploadCalls.push(new URL(request.url).search);
      return HttpResponse.json(uploadResult);
    });
    const user = userEvent.setup();
    renderWithProviders(<SettingsUpdateCenterTab />);
    await screen.findByText("Upload plugin");

    await pickFile(user);
    await user.click(screen.getByRole("button", { name: "Preview closure" }));

    await waitFor(() => expect(uploadCalls).toEqual(["?dryRun=true"]));
    expect(await screen.findByText("workflow-api")).toBeTruthy();
    expect(screen.getByText("satisfied-store")).toBeTruthy();
    expect(screen.getByText("planned-fetch")).toBeTruthy();
    // A preview reports what WOULD be downloaded.
    expect(screen.getByText(/would be downloaded/)).toBeTruthy();
    // Optional dependencies are reported, never resolved.
    expect(screen.getByText(/junit >= 1\.0/)).toBeTruthy();
    // lock-too-old is a warning, not a rejection.
    expect(screen.getByText(/lock-too-old/)).toBeTruthy();
  });

  it("renders the per-dependency table on a rejection", async () => {
    startServer(() => HttpResponse.json(rejectionBody, { status: 422 }));
    const user = userEvent.setup();
    renderWithProviders(<SettingsUpdateCenterTab />);
    await screen.findByText("Upload plugin");

    await pickFile(user);
    await user.click(screen.getByRole("button", { name: "Upload" }));

    expect(await screen.findByText("acme-internal")).toBeTruthy();
    expect(screen.getByText("not-in-store")).toBeTruthy();
    expect(screen.getByText(/seed this plugin via spec\.seed\.refs/)).toBeTruthy();
    expect(screen.getByText(/unresolved-dependencies/)).toBeTruthy();
  });

  it("refreshes the inventory after a committed upload", async () => {
    startServer(() => HttpResponse.json({ ...uploadResult, dryRun: false, packRef: "upload-9f2a1c0b3d4e" }, { status: 201 }));
    const user = userEvent.setup();
    renderWithProviders(<SettingsUpdateCenterTab />);
    await screen.findByText("Upload plugin");
    await waitFor(() => expect(pluginsCalls).toBeGreaterThan(0));
    const before = pluginsCalls;

    await pickFile(user);
    await user.click(screen.getByRole("button", { name: "Upload" }));

    expect(await screen.findByText(/Stored varroa-mcp-tools@1\.0\.0/)).toBeTruthy();
    await waitFor(() => expect(pluginsCalls).toBeGreaterThan(before));
  });
});
