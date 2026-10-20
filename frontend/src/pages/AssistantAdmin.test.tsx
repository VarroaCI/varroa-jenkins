import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { createTestQueryClient, renderWithProviders } from "../test/render-utils";
import { AssistantAdmin, pendingCountText } from "./AssistantAdmin";
import { PROVIDERS_FAILED } from "../components/assistant/providerChoice";
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

const SMOKE = { name: "smoke", kind: "openai-compatible", models: ["varroa-smoke"], trust: "internal", default: true, available: true, baseURL: "http://127.0.0.1:8082/v1" };

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings());
  vi.mocked(client.listAssistantProviders).mockResolvedValue({ items: [SMOKE] });
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

  it("shows Chart default and saves an empty default when none is set", async () => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ defaultProvider: "", defaultModel: "" }));
    renderAdmin();
    const select = (await screen.findByLabelText("Default provider")) as HTMLSelectElement;
    expect(select.value).toBe("");
    expect(select.selectedOptions[0].textContent).toBe("Chart default");
    fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ defaultProvider: "", defaultModel: "" }));
  });

  it("clears the default model", async () => {
    renderAdmin();
    fireEvent.change(await screen.findByLabelText("Default model"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ defaultProvider: "smoke", defaultModel: "" }));
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

describe("removed default provider", () => {
  const NOTICE = "The saved default provider gone is not available. Pick a listed provider or Chart default.";

  beforeEach(() => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ defaultProvider: "gone", defaultModel: "old-model" }));
  });

  it("names the removed default provider, shows a notice, and disables Save defaults", async () => {
    renderAdmin();
    const select = (await screen.findByLabelText("Default provider")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("gone"));
    expect(select.selectedOptions[0].textContent).toBe("gone (removed)");
    expect(await screen.findByText(NOTICE)).toBeTruthy();
    expect((screen.getByLabelText("Default model") as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save defaults" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("clears the stale model when a listed provider is picked, and saves it", async () => {
    renderAdmin();
    const select = (await screen.findByLabelText("Default provider")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("gone"));
    fireEvent.change(select, { target: { value: "smoke" } });
    expect(screen.queryByText(NOTICE)).toBeNull();
    expect((screen.getByLabelText("Default model") as HTMLInputElement).value).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ defaultProvider: "smoke", defaultModel: "" }));
  });

  it("clears the default model when the default provider changes between listed providers", async () => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ defaultProvider: "smoke", defaultModel: "varroa-smoke" }));
    vi.mocked(client.listAssistantProviders).mockResolvedValue({ items: [SMOKE, { ...SMOKE, name: "other", models: ["other-model"], default: false }] });
    renderAdmin();
    const select = (await screen.findByLabelText("Default provider")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("smoke"));
    fireEvent.change(select, { target: { value: "other" } });
    expect((screen.getByLabelText("Default model") as HTMLInputElement).value).toBe("");
    fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ defaultProvider: "other", defaultModel: "" }));
  });

  it("saves Chart default for a removed default provider", async () => {
    renderAdmin();
    const select = (await screen.findByLabelText("Default provider")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("gone"));
    fireEvent.change(select, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(client.putAssistantSettings).toHaveBeenCalledWith({ defaultProvider: "", defaultModel: "" }));
  });

  it("keeps the cached list and its notice when a background refresh fails", async () => {
    const queryClient = createTestQueryClient();
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ defaultProvider: "gone", defaultModel: "old-model" }));
    renderWithProviders(<AssistantAdmin />, { permissions: perms, route: "/administration/assistant", queryClient });
    const select = (await screen.findByLabelText("Default provider")) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe("gone"));
    vi.mocked(client.listAssistantProviders).mockRejectedValue(new ApiError(500, "boom"));
    await queryClient.refetchQueries({ queryKey: ["assistant-providers"] });
    expect(select.value).toBe("gone");
    expect(screen.getByText(NOTICE)).toBeTruthy();
    expect(screen.queryByText(PROVIDERS_FAILED)).toBeNull();
    expect((screen.getByRole("button", { name: "Save defaults" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows a notice when the provider list does not load", async () => {
    vi.mocked(client.getAssistantSettings).mockResolvedValue(createAssistantSettings({ defaultProvider: "smoke", defaultModel: "varroa-smoke" }));
    vi.mocked(client.listAssistantProviders).mockRejectedValue(new ApiError(500, "boom"));
    renderAdmin();
    expect(await screen.findByText(PROVIDERS_FAILED)).toBeTruthy();
    const select = (await screen.findByLabelText("Default provider")) as HTMLSelectElement;
    expect(select.value).toBe("smoke");
  });
});
