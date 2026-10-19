import { describe, it, expect, vi, beforeAll, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "../context/ThemeContext";
import { ToastProvider } from "./Toast";
import { AuthProvider } from "../context/AuthContext";
import { ComposerProvider } from "../context/ComposerContext";
import { createTestQueryClient } from "../test/render-utils";
import { installMockEventSource } from "../test/mockEventSource";
import Layout from "./Layout";
import * as client from "../api/client";
import { createMyAssistant } from "../test/assistantFactories";

vi.mock("../hooks/useApi", async (orig) => {
  const actual = await orig<typeof import("../hooks/useApi")>();
  return { ...actual, bffFetch: vi.fn(async () => ({ ticket: "t", expiresInSeconds: 30 })) };
});
vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return {
    ...actual,
    getMyAssistant: vi.fn(),
    getAssistantSettings: vi.fn(async () => ({ paused: false, pausedBy: null, pausedAt: null })),
    listAssistantSessions: vi.fn(async () => ({ items: [], nextCursor: null })),
  };
});

installMockEventSource();

function renderShell() {
  return render(
    <MemoryRouter initialEntries={["/activity"]}>
      <QueryClientProvider client={createTestQueryClient()}>
        <ThemeProvider>
          <ToastProvider>
            <AuthProvider>
              <ComposerProvider>
                <Routes>
                  <Route element={<Layout />}>
                    <Route path="/activity" element={<div>Activity page</div>} />
                  </Route>
                </Routes>
              </ComposerProvider>
            </AuthProvider>
          </ToastProvider>
        </ThemeProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  localStorage.clear();
  vi.mocked(client.getMyAssistant).mockReset();
});

describe("Layout assistant doors", () => {
  // Layout loads the drawer with React.lazy. The first load in a cold worker
  // can outlast the default findBy timeout, so the module loads before any test.
  beforeAll(async () => {
    await import("./assistant/AssistantDrawer");
  });

  it("shows no assistant door when the assistant is not available", async () => {
    vi.mocked(client.getMyAssistant).mockRejectedValue(new Error("403"));
    renderShell();
    await waitFor(() => expect(client.getMyAssistant).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Assistant" })).toBeNull();
  });

  it("opens the drawer from the top-bar button and closes it with the button again", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant());
    renderShell();
    const door = await screen.findByRole("button", { name: "Assistant" });
    fireEvent.click(door);
    expect(await screen.findByRole("complementary", { name: "Assistant" })).toBeTruthy();
    fireEvent.click(door);
    await waitFor(() => expect(screen.queryByRole("complementary", { name: "Assistant" })).toBeNull());
  });

  it("opens the drawer with the a key, and not while a text field has focus", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant());
    renderShell();
    await screen.findByRole("button", { name: "Assistant" });
    const field = document.createElement("textarea");
    document.body.appendChild(field);
    fireEvent.keyDown(field, { key: "a" });
    expect(screen.queryByRole("complementary", { name: "Assistant" })).toBeNull();
    field.remove();
    fireEvent.keyDown(document.body, { key: "a" });
    expect(await screen.findByRole("complementary", { name: "Assistant" })).toBeTruthy();
  });

  it("does not open the drawer with the a key when the assistant is off", async () => {
    vi.mocked(client.getMyAssistant).mockResolvedValue(createMyAssistant({ enabled: false }));
    renderShell();
    await waitFor(() => expect(client.getMyAssistant).toHaveBeenCalled());
    fireEvent.keyDown(document.body, { key: "a" });
    expect(screen.queryByRole("complementary", { name: "Assistant" })).toBeNull();
  });
});
