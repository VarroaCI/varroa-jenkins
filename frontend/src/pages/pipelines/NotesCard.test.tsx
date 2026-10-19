import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";
import { renderWithProviders } from "../../test/render-utils";
import { NotesCard } from "./NotesCard";
import { AssistantProvider } from "../../components/assistant/AssistantProvider";
import * as client from "../../api/client";
import { createRunNote, createMyAssistant } from "../../test/assistantFactories";

vi.mock("../../api/client", async (orig) => {
  const actual = await orig<typeof import("../../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    listRunNotes: vi.fn(),
    listJobNotes: vi.fn(),
    createRunNote: vi.fn(async () => createRunNote({ body: "posted" })),
    createAssistantSession: vi.fn(async () => ({ id: "s7" })),
  };
});

const perms = { global: { "*": { "*": true } } } as never;

function renderCard(node: React.ReactNode, route = "/pipelines/payments/payments%2Fapi/r1") {
  return renderWithProviders(<AssistantProvider>{node}</AssistantProvider>, { permissions: perms, route });
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant());
});

describe("NotesCard run mode", () => {
  it("lists notes and posts a human note from a writer", async () => {
    vi.mocked(client.listRunNotes).mockResolvedValue({ items: [createRunNote({ body: "looks flaky" })] });
    renderCard(<NotesCard mode="run" runId="r1" canWrite hasDigest={false} />);
    expect(await screen.findByText("looks flaky")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Write a note"), { target: { value: "a fix" } });
    expect(screen.getByText("5 / 16384")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Post" }));
    await waitFor(() => expect(client.createRunNote).toHaveBeenCalledWith("r1", { body: "a fix" }));
  });

  it("hides the composer from a reader", async () => {
    vi.mocked(client.listRunNotes).mockResolvedValue({ items: [] });
    renderCard(<NotesCard mode="run" runId="r1" canWrite={false} hasDigest={false} />);
    expect(await screen.findByText("No notes on this build.")).toBeTruthy();
    expect(screen.queryByLabelText("Write a note")).toBeNull();
  });

  it("names a note over the limit", async () => {
    vi.mocked(client.listRunNotes).mockResolvedValue({ items: [] });
    vi.mocked(client.createRunNote).mockRejectedValueOnce(Object.assign(new Error("long"), { status: 400, code: "note_too_long" }));
    renderCard(<NotesCard mode="run" runId="r1" canWrite hasDigest={false} />);
    fireEvent.change(await screen.findByLabelText("Write a note"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Post" }));
    expect(await screen.findByText(/The note was not saved|longer than 16384/)).toBeTruthy();
  });

  it("opens the profile tab when the assistant is switched off", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ enabled: false }));
    vi.mocked(client.listRunNotes).mockResolvedValue({ items: [] });
    renderCard(<NotesCard mode="run" runId="r1" canWrite={false} hasDigest />);
    fireEvent.click(await screen.findByRole("button", { name: /Explain this failure/ }));
    await waitFor(() => expect(client.createAssistantSession).not.toHaveBeenCalled());
  });

  it("starts an explain session when the assistant is on", async () => {
    vi.mocked(client.listRunNotes).mockResolvedValue({ items: [] });
    renderCard(<NotesCard mode="run" runId="r1" canWrite={false} hasDigest />);
    fireEvent.click(await screen.findByRole("button", { name: /Explain this failure/ }));
    await waitFor(() => expect(client.createAssistantSession).toHaveBeenCalled());
    expect(vi.mocked(client.createAssistantSession).mock.calls[0][0]).toMatchObject({ prompt: "explain-failure" });
  });

  it("shows Ask again and scrolls to the newest assistant answer", async () => {
    vi.mocked(client.listRunNotes).mockResolvedValue({
      items: [createRunNote({ id: "00000000-0000-7000-8000-0000000000e2", body: "answer", sessionId: "00000000-0000-7000-8000-000000000001", principal: "alice" })],
    });
    renderCard(<NotesCard mode="run" runId="r1" canWrite={false} hasDigest />);
    fireEvent.click(await screen.findByRole("button", { name: /Explain this failure/ }));
    expect(await screen.findByRole("button", { name: "Ask again" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Ask again" }));
    await waitFor(() => expect(client.createAssistantSession).toHaveBeenCalled());
  });

  it("labels an assistant note with its author", async () => {
    vi.mocked(client.listRunNotes).mockResolvedValue({
      items: [createRunNote({ sessionId: "00000000-0000-7000-8000-000000000001", body: "bot", principal: "alice" })],
    });
    renderCard(<NotesCard mode="run" runId="r1" canWrite={false} hasDigest={false} />);
    expect(await screen.findByText("Assistant answer for alice")).toBeTruthy();
  });
});

describe("NotesCard job mode", () => {
  it("lists job notes, linking run notes to their build", async () => {
    vi.mocked(client.listJobNotes).mockResolvedValue({
      items: [
        createRunNote({ runId: "00000000-0000-7000-8000-0000000000b1", buildNumber: 7, body: "run note" }),
        createRunNote({ runId: null, body: "job note" }),
      ],
    });
    renderCard(<NotesCard mode="job" team="payments" job="payments/api" />, "/pipelines/payments/payments%2Fapi");
    const link = await screen.findByRole("link", { name: "Build #7" });
    expect(link.getAttribute("href")).toContain("/pipelines/payments/payments%2Fapi/");
    expect(screen.getByText("Job note")).toBeTruthy();
    expect(screen.queryByLabelText("Write a note")).toBeNull();
  });

  it("renders nothing when the job has no notes", async () => {
    vi.mocked(client.listJobNotes).mockResolvedValue({ items: [] });
    const { container } = renderCard(<NotesCard mode="job" team="payments" job="payments/api" />);
    await waitFor(() => expect(client.listJobNotes).toHaveBeenCalled());
    expect(container.textContent).toBe("");
  });
});
