import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import { AssistantAdmin, pendingCountText } from "./AssistantAdmin";
import * as client from "../api/client";
import { ApiError } from "../hooks/useApi";
import { createAssistantApproval, createAssistantSettings } from "../test/assistantFactories";

vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return {
    ...actual,
    getAssistantSettings: vi.fn(),
    putAssistantSettings: vi.fn(async () => createAssistantSettings()),
    listAssistantApprovals: vi.fn(async () => ({ items: [createAssistantApproval()], nextCursor: null })),
    listAssistantProviders: vi.fn(async () => ({
      items: [{ name: "smoke", kind: "openai-compatible", models: ["varroa-smoke"], trust: "internal", default: true, available: true, baseURL: "http://127.0.0.1:8082/v1" }],
    })),
  };
});

const perms = { global: { "*": { "*": true } } } as never;

function renderAdmin() {
  return renderWithProviders(<AssistantAdmin />, { permissions: perms, route: "/administration/assistant" });
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings());
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

  it("lists providers and says the chart sets them", async () => {
    renderAdmin();
    expect(await screen.findByText("http://127.0.0.1:8082/v1")).toBeTruthy();
    expect(screen.getByText("The chart sets this list.")).toBeTruthy();
  });

  it("saves defaults and sets and removes a tenant level", async () => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ tenantDataSharing: { payments: "metadata" } }));
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ tenantDataSharing: { remove: "payments" } }));
    fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ defaultProvider: "smoke", defaultModel: "varroa-smoke" }));
    fireEvent.change(screen.getByLabelText("Tenant"), { target: { value: "checkout" } });
    fireEvent.change(screen.getByLabelText("Tenant level"), { target: { value: "logs" } });
    fireEvent.click(screen.getByRole("button", { name: "Set level" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ tenantDataSharing: { set: { tenant: "checkout", level: "logs" } } }));
  });

  it("states the limits and the egress, and says when the policy is off", async () => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({
      limits: { ...createAssistantSettings().limits, egress: { networkPolicy: false, cidrs: [], ports: [] } },
    }));
    renderAdmin();
    expect(await screen.findByText("The BFF egress is not pinned.")).toBeTruthy();
    expect(screen.getByText("Session budget 50000 tokens.")).toBeTruthy();
  });

  it("states the egress CIDRs and ports when the policy is on", async () => {
    renderAdmin();
    expect(await screen.findByText(/Network policy on\. CIDRs: 192\.168\.1\.99\/32\. Ports: 8082\./)).toBeTruthy();
  });

  it("explains a 503 when the assistant is off on the install", async () => {
    vi.mocked(client.getAssistantSettings).mockRejectedValue(new ApiError(503, "off"));
    renderAdmin();
    expect(await screen.findByText(/assistant\.enabled turns it on/)).toBeTruthy();
  });

  it("names a refused change", async () => {
    vi.mocked(client.putAssistantSettings).mockRejectedValueOnce(new ApiError(400, "bad", { error: "provider is unknown" }));
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Save defaults" }));
    expect(await screen.findByText("provider is unknown")).toBeTruthy();
  });
});
