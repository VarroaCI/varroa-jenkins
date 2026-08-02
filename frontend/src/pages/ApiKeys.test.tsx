import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import ApiKeys from "./ApiKeys";

const mockCreate = vi.fn();
const mockList = vi.fn();
const mockRevoke = vi.fn();
const mockRotate = vi.fn();

vi.mock("../api/client", () => ({
  createApiKey: (...args: unknown[]) => mockCreate(...args),
  listApiKeys: (...args: unknown[]) => mockList(...args),
  revokeApiKey: (...args: unknown[]) => mockRevoke(...args),
  rotateApiKey: (...args: unknown[]) => mockRotate(...args),
}));

const toastSpy = vi.fn();
vi.mock("../components/Toast", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../components/Toast")>();
  return { ...actual, useToast: () => ({ toast: toastSpy }) };
});

function renderPage(now = () => Date.parse("2026-07-12T12:00:00Z")) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const invalidate = vi.spyOn(queryClient, "invalidateQueries");
  return { ...render(<QueryClientProvider client={queryClient}><ApiKeys now={now} /></QueryClientProvider>), invalidate };
}

const key = (prefix: string, extra = {}) => ({ prefix, created: "2026-06-01T00:00:00Z", ...extra });

describe("ApiKeys", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockList.mockResolvedValue({ items: [] });
    Object.assign(navigator, { clipboard: { writeText: vi.fn().mockResolvedValue(undefined) } });
  });

  it("renders its standalone identity and empty state CTA", async () => {
    renderPage();
    expect(screen.getByRole("heading", { name: "API Keys" })).toBeInTheDocument();
    expect(screen.getByText(/long-lived credentials for automation/i)).toBeInTheDocument();
    expect(await screen.findByText("No API keys yet")).toBeInTheDocument();
    await userEvent.click(screen.getAllByRole("button", { name: "Create API key" })[1]);
    expect(screen.getAllByRole("button", { name: "Create API key" })[0]).toHaveFocus();
  });

  it("renders a semantic table with exact fallbacks, labels, timestamps, and statuses", async () => {
    const boundary = "2026-07-12T12:00:00Z";
    mockList.mockResolvedValue({ items: [
      key("equal", { name: "", expires: boundary }),
      key("future", { name: "deploy", lastUsed: "2026-07-01T10:30:00Z", expires: "2026-08-01T00:00:00Z" }),
      key("invalid", { expires: "not-a-date" }),
    ] });
    renderPage();
    const table = await screen.findByRole("table");
    expect(within(table).getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual(["Name", "Prefix", "Created", "Last used", "Expires", "Status", "Actions"]);
    expect(screen.getAllByText("Unnamed key")).toHaveLength(2);
    expect(screen.getAllByText("Never")).toHaveLength(2);
    expect(screen.getByText("Never expires")).toBeInTheDocument();
    expect(screen.getByText("equal").tagName).toBe("CODE");
    expect(screen.getByText("Expired")).toBeInTheDocument();
    expect(screen.getAllByText("Active")).toHaveLength(2);
    const created = document.querySelector('time[datetime="2026-06-01T00:00:00.000Z"]');
    expect(created).not.toBeNull();
    expect(created).toHaveAttribute("datetime", "2026-06-01T00:00:00.000Z");
    expect(created).toHaveAttribute("title", "2026-06-01T00:00:00.000Z");
    for (const label of ["Name", "Prefix", "Created", "Last used", "Expires", "Status", "Actions"]) {
      expect(table.querySelector(`td[data-label="${label}"]`)).not.toBeNull();
    }
  });

  it("retries a failed list request", async () => {
    mockList.mockRejectedValueOnce(new Error("offline")).mockResolvedValueOnce({ items: [] });
    renderPage();
    expect(await screen.findByText("API keys could not be loaded.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByText("No API keys yet")).toBeInTheDocument();
    expect(mockList).toHaveBeenCalledTimes(2);
  });

  it("creates, invalidates, copies, dismisses, and does not retain the secret after unmount", async () => {
    mockCreate.mockResolvedValue({ token: "vk_abc.secret", warning: "once" });
    const first = renderPage();
    await userEvent.type(screen.getByLabelText(/Key name/), "release");
    await userEvent.click(screen.getAllByRole("button", { name: "Create API key" })[0]);
    expect(await screen.findByText("vk_abc.secret")).toBeInTheDocument();
    expect(mockCreate).toHaveBeenCalledWith(undefined, "release");
    expect(first.invalidate).toHaveBeenCalledWith({ queryKey: ["apikeys"] });
    await userEvent.click(screen.getByRole("button", { name: "Copy to clipboard" }));
    expect(navigator.clipboard.writeText).toHaveBeenCalledWith("vk_abc.secret");
    await userEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("vk_abc.secret")).not.toBeInTheDocument();
    first.unmount();
    renderPage();
    expect(screen.queryByText("vk_abc.secret")).not.toBeInTheDocument();
  });

  it("keeps the create form and shows a safe inline error on failure", async () => {
    mockCreate.mockRejectedValue(new Error("secret backend detail"));
    renderPage();
    await userEvent.type(screen.getByLabelText(/Key name/), "release");
    await userEvent.click(screen.getAllByRole("button", { name: "Create API key" })[0]);
    expect(await screen.findByRole("alert")).toHaveTextContent("Failed to create key");
    expect(screen.getByLabelText(/Key name/)).toHaveValue("release");
    expect(screen.queryByText(/secret backend detail/)).not.toBeInTheDocument();
  });

  it("rotates a key, discloses the replacement, and reports failures without removing the row", async () => {
    mockList.mockResolvedValue({ items: [key("abc123")] });
    mockRotate.mockResolvedValueOnce({ token: "vk_new.secret" }).mockRejectedValueOnce(new Error("boom"));
    const { invalidate } = renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Rotate" }));
    expect(await screen.findByText("vk_new.secret")).toBeInTheDocument();
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["apikeys"] });
    await userEvent.click(screen.getByRole("button", { name: "Rotate" }));
    expect(await screen.findByText(/existing key remains active/i)).toBeInTheDocument();
    expect(screen.getAllByText("abc123")).not.toHaveLength(0);
  });

  it("cancels revoke with Cancel and Escape, restoring trigger focus without a request", async () => {
    mockList.mockResolvedValue({ items: [key("abc123")] });
    renderPage();
    const trigger = await screen.findByRole("button", { name: "Revoke" });
    await userEvent.click(trigger);
    const dialog = screen.getByRole("dialog", { name: "Revoke API key?" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(within(dialog).getByText("abc123")).toBeInTheDocument();
    expect(within(dialog).getByText(/permanent/i)).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Cancel" })).toHaveFocus();
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(trigger).toHaveFocus());
    await userEvent.click(trigger);
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(trigger).toHaveFocus());
    expect(mockRevoke).not.toHaveBeenCalled();
  });

  it("confirms revoke once, invalidates, and moves focus to the next row", async () => {
    mockList.mockResolvedValue({ items: [key("first"), key("second")] });
    mockRevoke.mockResolvedValue(undefined);
    const { invalidate } = renderPage();
    const triggers = await screen.findAllByRole("button", { name: "Revoke" });
    await userEvent.click(triggers[0]);
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(mockRevoke).toHaveBeenCalledTimes(1));
    expect(mockRevoke).toHaveBeenCalledWith("first");
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["apikeys"] });
    await waitFor(() => expect(triggers[1]).toHaveFocus());
  });

  it("keeps revoke confirmation and row visible after failure", async () => {
    mockList.mockResolvedValue({ items: [key("abc123")] });
    mockRevoke.mockRejectedValue(new Error("private detail"));
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Revoke" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Revoke" }));
    expect(await within(screen.getByRole("dialog")).findByRole("alert")).toHaveTextContent("key remains active");
    expect(screen.getAllByText("abc123")).not.toHaveLength(0);
  });
});
