import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import { ProfileAssistantTab, keyProviderFor, keyStatus } from "./ProfileAssistantTab";
import { PROVIDERS_FAILED } from "../components/assistant/providerChoice";
import { AssistantProvider } from "../components/assistant/AssistantProvider";
import * as client from "../api/client";
import { ApiError } from "../hooks/useApi";
import { createMyAssistant } from "../test/assistantFactories";

vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    listAssistantProviders: vi.fn(async () => ({
      items: [{ name: "smoke", kind: "openai-compatible", models: ["varroa-smoke"], trust: "internal", default: true, available: true, baseURL: "http://x" }],
    })),
    putMyAssistant: vi.fn(async () => createMyAssistant()),
    putMyAssistantKey: vi.fn(async () => createMyAssistant({ sessionKey: { provider: "smoke" } })),
    deleteMyAssistantKey: vi.fn(async () => undefined),
  };
});

const perms = { global: { "*": { "*": true } } } as never;

const SMOKE = { name: "smoke", kind: "openai-compatible", models: ["varroa-smoke"], trust: "internal", default: true, available: true, baseURL: "http://x" };

function renderTab(route = "/profile?tab=assistant", state?: unknown) {
  return renderWithProviders(
    <AssistantProvider>
      <ProfileAssistantTab />
    </AssistantProvider>,
    { permissions: perms, route, routerState: state },
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
  window.sessionStorage.clear();
  vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant());
  vi.mocked(client.listAssistantProviders).mockResolvedValue({ items: [SMOKE] });
});

describe("ProfileAssistantTab", () => {
  it("saves the switch with all four fields", async () => {
    renderTab();
    const sw = await screen.findByRole("switch", { name: "Assistant on" });
    await waitFor(() => expect((sw as HTMLInputElement).checked).toBe(true));
    fireEvent.click(sw);
    await waitFor(() => expect(client.putMyAssistant).toHaveBeenCalledWith(expect.objectContaining({
      enabled: false, provider: "smoke", model: "varroa-smoke", dataSharing: "logs",
    })));
  });

  it("shows the server message when a save is refused", async () => {
    vi.mocked(client.putMyAssistant).mockRejectedValueOnce(new ApiError(400, "bad", { error: "model is not offered" }));
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Save" }));
    expect(await screen.findByText("model is not offered")).toBeTruthy();
  });

  it("states the spend today", async () => {
    renderTab();
    expect(await screen.findByText("0 of 100000 tokens used today (UTC)")).toBeTruthy();
  });

  it("explains the explain notice when opened from a failed build", async () => {
    renderTab("/profile?tab=assistant", { from: "explain" });
    expect(await screen.findByText("Turn on the assistant to explain a failed build.")).toBeTruthy();
  });

  it("sets a session key directly and clears the field, with no key in any storage or cache", async () => {
    renderTab();
    const input = await screen.findByLabelText("API key");
    await waitFor(() => expect((screen.getByLabelText("Key provider") as HTMLSelectElement).value).toBe("smoke"));
    fireEvent.change(input, { target: { value: "sk-test-123" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    await waitFor(() => expect(client.putMyAssistantKey).toHaveBeenCalledWith({ provider: "smoke", apiKey: "sk-test-123" }));
    await waitFor(() => expect((screen.getByLabelText("API key") as HTMLInputElement).value).toBe(""));
    expect(JSON.stringify(window.localStorage)).not.toContain("sk-test-123");
    expect(JSON.stringify(window.sessionStorage)).not.toContain("sk-test-123");
  });

  it("explains a 409 for an API key session", async () => {
    vi.mocked(client.putMyAssistantKey).mockRejectedValueOnce(new ApiError(409, "conflict"));
    renderTab();
    fireEvent.change(await screen.findByLabelText("API key"), { target: { value: "k" } });
    await waitFor(() => expect((screen.getByLabelText("Key provider") as HTMLSelectElement).value).toBe("smoke"));
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    expect(await screen.findByText("A key needs a browser login. An API key session cannot hold one.")).toBeTruthy();
  });

  it("names the unavailable key error", async () => {
    vi.mocked(client.putMyAssistantKey).mockRejectedValueOnce(new ApiError(503, "x", { code: "assistant_key_unavailable" }));
    renderTab();
    fireEvent.change(await screen.findByLabelText("API key"), { target: { value: "k" } });
    await waitFor(() => expect((screen.getByLabelText("Key provider") as HTMLSelectElement).value).toBe("smoke"));
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    expect(await screen.findByText(/not available in this login/)).toBeTruthy();
  });

  it("clears the key and reports the status", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ sessionKey: { provider: "smoke" } }));
    renderTab();
    expect(await screen.findByText("Key set for smoke, held for this login only")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Clear key" }));
    await waitFor(() => expect(client.deleteMyAssistantKey).toHaveBeenCalled());
  });

  it("reports no key when none is set", () => {
    expect(keyStatus(createMyAssistant())).toBe("No key set");
  });

  it("starts the key provider on the default provider when none is saved, and sends it", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ provider: "", model: "" }));
    vi.mocked(client.listAssistantProviders).mockResolvedValue({
      items: [
        { ...SMOKE, name: "alpha", default: false },
        { ...SMOKE, name: "beta", default: true },
      ],
    });
    renderTab();
    await waitFor(() => expect((screen.getByLabelText("Key provider") as HTMLSelectElement).value).toBe("beta"));
    fireEvent.change(screen.getByLabelText("API key"), { target: { value: "sk-x" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    await waitFor(() => expect(client.putMyAssistantKey).toHaveBeenCalledWith({ provider: "beta", apiKey: "sk-x" }));
  });

  it("starts the key provider on the first provider when none is default", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ provider: "", model: "" }));
    vi.mocked(client.listAssistantProviders).mockResolvedValue({
      items: [
        { ...SMOKE, name: "alpha", default: false },
        { ...SMOKE, name: "beta", default: false },
      ],
    });
    renderTab();
    await waitFor(() => expect((screen.getByLabelText("Key provider") as HTMLSelectElement).value).toBe("alpha"));
    fireEvent.change(screen.getByLabelText("API key"), { target: { value: "sk-x" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    await waitFor(() => expect(client.putMyAssistantKey).toHaveBeenCalledWith({ provider: "alpha", apiKey: "sk-x" }));
  });

  it("disables Set key when no provider is listed", async () => {
    vi.mocked(client.listAssistantProviders).mockResolvedValue({ items: [] });
    renderTab();
    fireEvent.change(await screen.findByLabelText("API key"), { target: { value: "sk-x" } });
    expect((screen.getByRole("button", { name: "Set key" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("shows Default on the model card when no provider is saved and saves it empty", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ provider: "", model: "" }));
    renderTab();
    const select = (await screen.findByLabelText("Provider")) as HTMLSelectElement;
    expect(select.value).toBe("");
    expect(select.selectedOptions[0].textContent).toBe("Default");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(client.putMyAssistant).toHaveBeenCalledWith(expect.objectContaining({ provider: "", model: "" })));
  });

  describe("keyProviderFor", () => {
    const list = [
      { ...SMOKE, name: "alpha", default: false },
      { ...SMOKE, name: "beta", default: true },
    ];

    it("keeps a chosen provider that is listed", () => {
      expect(keyProviderFor("alpha", "beta", list)).toBe("alpha");
    });

    it("falls back to the saved provider when the chosen one is not listed", () => {
      expect(keyProviderFor("gone", "alpha", list)).toBe("alpha");
    });

    it("falls back to the default provider when the saved one is not listed", () => {
      expect(keyProviderFor("", "gone", list)).toBe("beta");
    });

    it("falls back to the first provider when none is default", () => {
      expect(keyProviderFor("", "", [{ ...SMOKE, name: "alpha", default: false }])).toBe("alpha");
    });

    it("returns an empty name when no provider is listed", () => {
      expect(keyProviderFor("", "", [])).toBe("");
    });
  });

  describe("provider list that does not load", () => {
    beforeEach(() => {
      vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ provider: "smoke", model: "varroa-smoke" }));
      vi.mocked(client.listAssistantProviders).mockRejectedValue(new ApiError(500, "boom"));
    });

    afterEach(() => {
      vi.mocked(client.listAssistantProviders).mockResolvedValue({ items: [SMOKE] });
    });

    it("shows a notice and keeps the saved provider and model in view", async () => {
      renderTab();
      expect(await screen.findByText(PROVIDERS_FAILED)).toBeTruthy();
      await waitFor(() => expect((screen.getByLabelText("Provider") as HTMLSelectElement).value).toBe("smoke"));
      const model = screen.getByLabelText("Model") as HTMLSelectElement;
      expect(model.value).toBe("varroa-smoke");
      expect(model.disabled).toBe(true);
    });

    it("still saves the saved values", async () => {
      renderTab();
      expect(await screen.findByText(PROVIDERS_FAILED)).toBeTruthy();
      const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
      expect(save.disabled).toBe(false);
      fireEvent.click(save);
      await waitFor(() => expect(client.putMyAssistant).toHaveBeenCalledWith(expect.objectContaining({ provider: "smoke", model: "varroa-smoke" })));
    });
  });

  describe("removed saved provider", () => {
    const NOTICE = "The saved provider gone is not available. Pick a provider or Default.";

    beforeEach(() => {
      vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ provider: "gone", model: "old-model" }));
    });

    it("names a saved provider that the chart no longer lists and shows a notice", async () => {
      renderTab();
      const select = (await screen.findByLabelText("Provider")) as HTMLSelectElement;
      await waitFor(() => expect(select.value).toBe("gone"));
      expect(select.selectedOptions[0].textContent).toBe("gone (removed)");
      expect(await screen.findByText(NOTICE)).toBeTruthy();
      expect((screen.getByLabelText("Model") as HTMLSelectElement).disabled).toBe(true);
      expect((screen.getByRole("button", { name: "Save" }) as HTMLButtonElement).disabled).toBe(true);
    });

    it("enables the model select after a provider is picked for a removed saved provider", async () => {
      renderTab();
      const select = (await screen.findByLabelText("Provider")) as HTMLSelectElement;
      await waitFor(() => expect(select.value).toBe("gone"));
      fireEvent.change(select, { target: { value: "smoke" } });
      await waitFor(() => expect(screen.queryByText(NOTICE)).toBeNull());
      const model = screen.getByLabelText("Model") as HTMLSelectElement;
      expect(model.disabled).toBe(false);
      expect(model.value).toBe("varroa-smoke");
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await waitFor(() => expect(client.putMyAssistant).toHaveBeenCalledWith(expect.objectContaining({ provider: "smoke", model: "varroa-smoke" })));
    });

    it("saves Default when Default is picked for a removed saved provider", async () => {
      renderTab();
      const select = (await screen.findByLabelText("Provider")) as HTMLSelectElement;
      await waitFor(() => expect(select.value).toBe("gone"));
      fireEvent.change(select, { target: { value: "" } });
      await waitFor(() => expect(screen.queryByText(NOTICE)).toBeNull());
      fireEvent.click(screen.getByRole("button", { name: "Save" }));
      await waitFor(() => expect(client.putMyAssistant).toHaveBeenCalledWith(expect.objectContaining({ provider: "", model: "" })));
    });

    it("sends Default with the switch when the saved provider is removed", async () => {
      renderTab();
      expect(await screen.findByText(NOTICE)).toBeTruthy();
      fireEvent.click(screen.getByRole("switch", { name: "Assistant on" }));
      await waitFor(() => expect(client.putMyAssistant).toHaveBeenCalledWith(expect.objectContaining({ enabled: false, provider: "", model: "" })));
    });
  });
});
