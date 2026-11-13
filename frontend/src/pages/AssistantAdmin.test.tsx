import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import { AssistantAdmin, CONFIG_CHANGED, pendingCountText } from "./AssistantAdmin";
import * as client from "../api/client";
import { ApiError } from "../hooks/useApi";
import { createAssistantApproval, createAssistantConfig, createAssistantSettings } from "../test/assistantFactories";

vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return {
    ...actual,
    getAssistantSettings: vi.fn(),
    putAssistantSettings: vi.fn(async () => createAssistantSettings()),
    getAssistantConfig: vi.fn(),
    putAssistantConfig: vi.fn(async () => createAssistantConfig()),
    listAssistantApprovals: vi.fn(async () => ({ items: [createAssistantApproval()], nextCursor: null })),
  };
});

const perms = { global: { "*": { "*": true } } } as never;

function renderAdmin() {
  return renderWithProviders(<AssistantAdmin />, { permissions: perms, route: "/administration/assistant" });
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings());
  vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig());
});

describe("AssistantAdmin", () => {
  it("pauses only after a confirmation", async () => {
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Pause" }));
    expect(screen.getByRole("alertdialog", { name: "Confirm pause" })).toBeTruthy();
    expect(client.putAssistantSettings).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Confirm pause" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ paused: true }));
  });

  it("resumes a paused assistant", async () => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ paused: true, pausedBy: "local:admin", pausedAt: "2026-10-18T10:00:00Z" }));
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Resume" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ paused: false }));
  });

  it("shows the pending count and says or more when a next page exists", async () => {
    vi.mocked(client.listAssistantApprovals).mockResolvedValue({ items: [createAssistantApproval()], nextCursor: "next" });
    renderAdmin();
    expect(await screen.findByText("1 or more pending")).toBeTruthy();
    expect(pendingCountText(3, false)).toBe("3");
  });

  it("lists the providers with their auth, CIDRs, and port, and names the object", async () => {
    renderAdmin();
    expect(await screen.findByText("http://192.168.1.99:8082/v1")).toBeTruthy();
    expect(screen.getByText("192.168.1.99/32")).toBeTruthy();
    expect(screen.getByText("Edit the AssistantConfig object varroa-assistant to change this list.")).toBeTruthy();
  });

  it("shows the egress condition, each provider's rule, and the key state", async () => {
    const base = createAssistantConfig();
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig({
      spec: { ...base.spec, providers: [{ ...base.spec.providers[0], auth: "apiKey" }] },
      status: {
        providers: [{ name: "smoke", keySet: true }],
        conditions: [{ type: "EgressReady", status: "True", reason: "EgressRendered", message: "every provider has an egress rule" }],
      },
    }));
    renderAdmin();
    expect(await screen.findByText("smoke: 192.168.1.99/32, port 8082")).toBeTruthy();
    expect(screen.getByText("every provider has an egress rule")).toBeTruthy();
    expect(screen.getByText("set")).toBeTruthy();
  });

  it("names the provider that has no egress rule", async () => {
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig({
      status: {
        conditions: [{ type: "EgressReady", status: "False", reason: "ProviderEgressMissing", message: "no CIDRs for provider(s): smoke" }],
      },
    }));
    renderAdmin();
    expect(await screen.findByText(/Some providers have no egress rule/)).toBeTruthy();
  });

  it("sets and removes a tenant level", async () => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ tenantDataSharing: { payments: "metadata" } }));
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ tenantDataSharing: { remove: "payments" } }));
    fireEvent.change(screen.getByLabelText("Tenant"), { target: { value: "checkout" } });
    fireEvent.change(screen.getByLabelText("Tenant level"), { target: { value: "logs" } });
    fireEvent.click(screen.getByRole("button", { name: "Set level" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ tenantDataSharing: { set: { tenant: "checkout", level: "logs" } } }));
  });

  it("saves the defaults as a whole object with its resourceVersion", async () => {
    renderAdmin();
    fireEvent.change(await screen.findByLabelText("Default model"), { target: { value: "varroa-smoke" } });
    fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(client.putAssistantConfig).toHaveBeenCalledWith(expect.objectContaining({
      metadata: expect.objectContaining({ name: "varroa-assistant", resourceVersion: "7" }),
      spec: expect.objectContaining({ defaultProvider: "smoke", defaultModel: "varroa-smoke", budgets: expect.any(Object) }),
    })));
  });

  it("shows the limits from the object", async () => {
    renderAdmin();
    expect(await screen.findByText("Session budget")).toBeTruthy();
    expect(screen.getByText("50000 tokens")).toBeTruthy();
    expect(screen.getByText("1h")).toBeTruthy();
  });

  it("shows the reload message when another save won the version fence", async () => {
    vi.mocked(client.putAssistantConfig).mockRejectedValueOnce(new ApiError(409, "conflict", { error: "stale" }));
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Save defaults" }));
    expect(await screen.findByText(CONFIG_CHANGED)).toBeTruthy();
  });

  it("names a refused settings change", async () => {
    vi.mocked(client.putAssistantSettings).mockRejectedValueOnce(new ApiError(400, "bad", { error: "tenant is unknown" }));
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Pause" }));
    fireEvent.click(screen.getByRole("button", { name: "Confirm pause" }));
    expect(await screen.findByText("tenant is unknown")).toBeTruthy();
  });

  it("offers Retry when the record store does not answer", async () => {
    vi.mocked(client.getAssistantSettings).mockRejectedValueOnce(
      new ApiError(503, "unreachable", { error: "record store unreachable", code: "record_store_unreachable" }),
    );
    renderAdmin();
    expect(await screen.findByRole("alert")).toHaveTextContent("Varroa cannot reach the record store.");
    expect(screen.queryByText(/assistant\.enabled turns it on/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("The assistant is running.")).toBeTruthy();
  });

  it("explains a 503 when the assistant is off on the install", async () => {
    vi.mocked(client.getAssistantSettings).mockRejectedValue(new ApiError(503, "off"));
    renderAdmin();
    expect(await screen.findByText(/assistant\.enabled turns it on/)).toBeTruthy();
  });

  it("shows a configuration failure with a Retry action instead of hiding the cards", async () => {
    vi.mocked(client.getAssistantConfig).mockRejectedValue(new ApiError(503, "operator unavailable"));
    renderAdmin();
    expect(await screen.findByText(/The assistant configuration did not load/)).toBeTruthy();
    expect(screen.queryByText("Providers")).toBeNull();
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig());
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("Providers")).toBeTruthy();
  });
});
