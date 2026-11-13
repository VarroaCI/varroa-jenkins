import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../test/render-utils";
import { AssistantAdmin, CONFIG_CHANGED, pendingCountText } from "./AssistantAdmin";
import * as client from "../api/client";
import { ApiError } from "../hooks/useApi";
import { createAssistantApproval, createAssistantConfig, createAssistantSettings } from "../test/assistantFactories";
import { keyFailure } from "./assistantAdmin/ProviderKeyForm";

vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return {
    ...actual,
    getAssistantSettings: vi.fn(),
    putAssistantSettings: vi.fn(async () => createAssistantSettings()),
    getAssistantConfig: vi.fn(),
    putAssistantConfig: vi.fn(async () => createAssistantConfig()),
    putAssistantProviderKey: vi.fn(async () => undefined),
    deleteAssistantProviderKey: vi.fn(async () => undefined),
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

  it("lists the providers with their auth, CIDRs, and port, and marks the default", async () => {
    renderAdmin();
    expect(await screen.findByText("http://192.168.1.99:8082/v1")).toBeTruthy();
    expect(screen.getByText("192.168.1.99/32")).toBeTruthy();
    expect(screen.getByText("smoke (default)")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Add provider" })).toBeTruthy();
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

  it("shows the limits from the object in editable fields", async () => {
    renderAdmin();
    expect(await screen.findByLabelText("Session budget")).toHaveValue("50000");
    expect(screen.getByLabelText("Approval expiry")).toHaveValue("1h");
    expect(screen.getByLabelText("External data sharing")).toHaveValue("logs");
  });

  it("adds a provider and sends the whole list at the read version", async () => {
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "backup" } });
    fireEvent.change(screen.getByLabelText("Base URL"), { target: { value: "https://models.test/v1" } });
    fireEvent.change(screen.getByLabelText("Models"), { target: { value: "model-c" } });
    fireEvent.change(screen.getByLabelText("CIDRs"), { target: { value: "192.0.2.0/24" } });
    fireEvent.change(screen.getByLabelText("Port"), { target: { value: "443" } });
    fireEvent.click(screen.getByRole("button", { name: "Save provider" }));
    await waitFor(() => expect(client.putAssistantConfig).toHaveBeenCalledWith({
      apiVersion: "varroa.dev/v1alpha1",
      kind: "AssistantConfig",
      metadata: { name: "varroa-assistant", resourceVersion: "7" },
      spec: expect.objectContaining({
        providers: [
          expect.objectContaining({ name: "smoke" }),
          { name: "backup", kind: "openai-compatible", baseURL: "https://models.test/v1", models: ["model-c"], trust: "external", auth: "apiKey", cidrs: ["192.0.2.0/24"], port: 443 },
        ],
        defaultProvider: "smoke",
      }),
    }));
  });

  it("shows a field fault before any save", async () => {
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Add provider" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Bad Name" } });
    fireEvent.click(screen.getByRole("button", { name: "Save provider" }));
    expect(await screen.findByText(/Use up to 63 lowercase letters/)).toBeTruthy();
    expect(client.putAssistantConfig).not.toHaveBeenCalled();
  });

  it("removing the default provider asks for a new default in the same save", async () => {
    const base = createAssistantConfig();
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig({
      spec: {
        ...base.spec,
        providers: [
          base.spec.providers[0],
          { name: "backup", kind: "anthropic", models: ["model-c", "model-d"], trust: "external", auth: "none" },
        ],
      },
    }));
    renderAdmin();
    fireEvent.click(await screen.findByRole("button", { name: "Remove smoke" }));
    const dialog = screen.getByRole("alertdialog", { name: "Remove provider smoke" });
    expect(dialog).toBeTruthy();
    fireEvent.change(screen.getByLabelText("New default provider"), { target: { value: "backup" } });
    fireEvent.click(screen.getByRole("button", { name: "Remove provider" }));
    await waitFor(() => expect(client.putAssistantConfig).toHaveBeenCalledWith(expect.objectContaining({
      metadata: expect.objectContaining({ resourceVersion: "7" }),
      spec: expect.objectContaining({
        providers: [expect.objectContaining({ name: "backup" })],
        defaultProvider: "backup",
        defaultModel: "model-c",
      }),
    })));
  });

  it("saves a provider key, clears the input, and never reads it back", async () => {
    const base = createAssistantConfig();
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig({
      spec: { ...base.spec, providers: [{ ...base.spec.providers[0], auth: "apiKey" }] },
      status: { providers: [{ name: "smoke", keySet: false }] },
    }));
    renderAdmin();
    expect(await screen.findByText("not set")).toBeTruthy();
    const input = screen.getByLabelText("API key for smoke");
    expect(input).toHaveAttribute("type", "password");
    fireEvent.change(input, { target: { value: "sk-test-123" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    await waitFor(() => expect(client.putAssistantProviderKey).toHaveBeenCalledWith("smoke", { apiKey: "sk-test-123" }));
    await waitFor(() => expect(screen.getByLabelText("API key for smoke")).toHaveValue(""));
    expect(screen.queryByText("sk-test-123")).toBeNull();
  });

  it("clears the key input after a refused key save too", async () => {
    const base = createAssistantConfig();
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig({
      spec: { ...base.spec, providers: [{ ...base.spec.providers[0], auth: "apiKey" }] },
    }));
    vi.mocked(client.putAssistantProviderKey).mockRejectedValueOnce(new ApiError(500, "boom"));
    renderAdmin();
    fireEvent.change(await screen.findByLabelText("API key for smoke"), { target: { value: "sk-refused-456" } });
    fireEvent.click(screen.getByRole("button", { name: "Set key" }));
    expect(await screen.findByText("The key was not saved.")).toBeTruthy();
    expect(screen.getByLabelText("API key for smoke")).toHaveValue("");
    expect(screen.queryByText("sk-refused-456")).toBeNull();
  });

  it("asks for a confirmation when external data sharing rises", async () => {
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig({
      spec: { ...createAssistantConfig().spec, externalDataSharing: "none" },
    }));
    renderAdmin();
    fireEvent.change(await screen.findByLabelText("External data sharing"), { target: { value: "logs" } });
    fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
    expect(await screen.findByText("External data sharing rises from none to logs. External providers may then receive logs data.")).toBeTruthy();
    expect(client.putAssistantConfig).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Confirm save" }));
    await waitFor(() => expect(client.putAssistantConfig).toHaveBeenCalledWith(expect.objectContaining({
      spec: expect.objectContaining({ externalDataSharing: "logs" }),
    })));
  });

  it("names a stale provider on a key refusal and a format fault on invalid_key", () => {
    expect(keyFailure(new ApiError(400, "bad", { error: "the provider is not listed in the assistant config", code: "unknown_provider" })))
      .toBe("the provider is not listed in the assistant config. Reload the page to see the current providers.");
    expect(keyFailure(new ApiError(400, "bad", { error: "the key must be 1 to 4096 bytes", code: "invalid_key" })))
      .toBe("The key must be 1 to 4096 bytes and not only white space.");
    expect(keyFailure(new ApiError(500, "boom"))).toBe("The key was not saved.");
  });

  it("Back on a confirmation dismisses it without saving", async () => {
    vi.mocked(client.getAssistantConfig).mockResolvedValue(createAssistantConfig({
      spec: { ...createAssistantConfig().spec, externalDataSharing: "none" },
    }));
    renderAdmin();
    fireEvent.change(await screen.findByLabelText("External data sharing"), { target: { value: "logs" } });
    fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
    fireEvent.click(await screen.findByRole("button", { name: "Back" }));
    expect(screen.queryByRole("alertdialog", { name: "Confirm the limit change" })).toBeNull();
    expect(client.putAssistantConfig).not.toHaveBeenCalled();
  });

  it("shows the server message next to the form when a save is refused", async () => {
    vi.mocked(client.putAssistantConfig).mockRejectedValueOnce(new ApiError(400, "bad", { error: "budgets.session must not be negative" }));
    renderAdmin();
    fireEvent.change(await screen.findByLabelText("Session budget"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save limits" }));
    expect(await screen.findByText("budgets.session must not be negative")).toBeTruthy();
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
