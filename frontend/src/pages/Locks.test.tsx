import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { ApiError } from "../hooks/useApi";
import { createFleetLock } from "../test/factories";
import Locks from "./Locks";

const mockListAllFleetLocks = vi.fn();
vi.mock("../api/client", () => ({
  listAllFleetLocks: (...args: unknown[]) => mockListAllFleetLocks(...args),
}));

/** The page opens no stream, so this spy stays uncalled. */
const eventSourceSpy = vi.fn();

function renderPage() {
  return render(
    <MemoryRouter>
      <QueryClientProvider client={createTestQueryClient()}>
        <Locks />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockListAllFleetLocks.mockReset();
  eventSourceSpy.mockClear();
  vi.stubGlobal("EventSource", eventSourceSpy);
});

describe("Locks", () => {
  it("lists every tenant's locks for a caller holding fleetlocks:read", async () => {
    mockListAllFleetLocks.mockResolvedValue({
      items: [
        createFleetLock({ name: "payments/smoke-lock", tenant: "payments", state: "held" }),
        createFleetLock({ name: "search/smoke-lock", tenant: "search", state: "waiting" }),
      ],
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("payments/smoke-lock")).toBeInTheDocument());
    expect(screen.getByText("search/smoke-lock")).toBeInTheDocument();
    expect(screen.getByText("search")).toBeInTheDocument();
  });

  it("states that a waiting row is held elsewhere when no holder row is present", async () => {
    mockListAllFleetLocks.mockResolvedValue({
      items: [createFleetLock({ name: "payments/smoke-lock", state: "waiting" })],
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("held elsewhere")).toBeInTheDocument());
  });

  it("reads a held row's elapsed time from acquiredAt and a waiting row's from waitingSince", async () => {
    const ago = (mins: number) => new Date(Date.now() - mins * 60_000).toISOString();
    mockListAllFleetLocks.mockResolvedValue({
      items: [
        createFleetLock({ name: "payments/a-lock", state: "held", acquiredAt: ago(5) }),
        createFleetLock({ name: "payments/a-lock", state: "waiting", waitingSince: ago(3) }),
        createFleetLock({ name: "payments/b-lock", state: "held", acquiredAt: undefined }),
      ],
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("5m ago")).toBeInTheDocument());
    expect(screen.getByText("3m ago")).toBeInTheDocument();
    const bRow = screen.getByText("payments/b-lock").closest("tr");
    expect(bRow?.lastElementChild?.textContent).toBe("—");
  });

  it("renders the forbidden state for a refused read", async () => {
    mockListAllFleetLocks.mockRejectedValue(new ApiError(403, "no"));
    renderPage();

    await waitFor(() => expect(screen.getByText("Access denied")).toBeInTheDocument());
  });

  it("renders the not-found state for an unknown read", async () => {
    mockListAllFleetLocks.mockRejectedValue(new ApiError(404, "missing"));
    renderPage();

    await waitFor(() => expect(screen.getByText("Not found")).toBeInTheDocument());
  });

  it("polls the read and opens no stream", async () => {
    mockListAllFleetLocks.mockResolvedValue({ items: [] });
    renderPage();

    await waitFor(() => expect(mockListAllFleetLocks).toHaveBeenCalledTimes(1));
    expect(eventSourceSpy).not.toHaveBeenCalled();
  });
});
