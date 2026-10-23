import { describe, it, expect, vi, beforeEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import { createFleetLock } from "../../test/factories";
import type { RunNotification } from "../../types";
import TenantHome from "./TenantHome";

const mockListPipelines = vi.fn();
const mockListRunners = vi.fn();
const mockGetTenantCell = vi.fn();
const mockListFleetLocks = vi.fn();
const mockGetTenant = vi.fn();
vi.mock("../../api/client", () => ({
  getMyPermissions: async () => ({ global: {}, scopes: [], tenants: [] }),
  listPipelines: (...args: unknown[]) => mockListPipelines(...args),
  getTenant: (...args: unknown[]) => mockGetTenant(...args),
  listRunners: (...args: unknown[]) => mockListRunners(...args),
  getTenantCell: (...args: unknown[]) => mockGetTenantCell(...args),
  listFleetLocks: (...args: unknown[]) => mockListFleetLocks(...args),
  tenantRunStreamUrl: (tenant: string) => `/api/v1/pipelines/${encodeURIComponent(tenant)}/stream`,
}));

/** RunEvent holds one frame the page reads off the tenant stream. */
type RunEvent = { type: string; data: RunNotification };

const mockUseEventStream = vi.fn(
  (_baseUrl?: string | null, _scope?: string | null, _eventNames?: string[]) => ({
    lastEvent: null as RunEvent | null,
    readyState: "closed",
    error: null as Error | null,
  }),
);
vi.mock("../../hooks/useEventStream", () => ({
  useEventStream: (...args: Parameters<typeof mockUseEventStream>) => mockUseEventStream(...args),
}));

/** frame builds one stream frame of the tenant's own stream. */
function frame(type: "run" | "stage" | "lock"): RunEvent {
  return {
    type,
    data: {
      kind: type,
      cluster: "core",
      tenant: "payments",
      job_path: "payments/checkout-api",
      run_id: "run-1",
      seq: 1,
      at: "2026-01-01T00:00:00Z",
    },
  };
}

/** renderPage returns repaint, which re-renders the page so it reads the
 * stream state the test just set. */
function renderPage(route = "/pipelines/payments") {
  const queryClient = createTestQueryClient();
  const tree = () => (
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={queryClient}>
        <Routes>
          <Route path="/pipelines/:tenant" element={<TenantHome />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>
  );
  const view = render(tree());
  return { ...view, repaint: () => view.rerender(tree()) };
}

beforeEach(() => {
  mockUseEventStream.mockClear();
  mockUseEventStream.mockReturnValue({ lastEvent: null, readyState: "closed", error: null });
  mockListPipelines.mockReset();
  mockListRunners.mockReset();
  mockGetTenantCell.mockReset();
  mockListFleetLocks.mockReset();
  mockListPipelines.mockResolvedValue({ items: [], runningNow: [], tenantFolder: "payments", truncated: false });
  mockListRunners.mockResolvedValue([]);
  mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
  mockListFleetLocks.mockResolvedValue({ items: [] });
});

describe("TenantHome — the Locks card", () => {
  it("shows a held row's own run link", async () => {
    mockListFleetLocks.mockResolvedValue({
      items: [
        createFleetLock({
          name: "payments/smoke-lock",
          state: "held",
          jobPath: "payments/holder-job",
          runLink: "/pipelines/payments/payments%2Fholder-job/run-1",
        }),
      ],
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("payments/smoke-lock")).toBeInTheDocument());
    const link = screen.getByRole("link", { name: /holder-job/ });
    expect(link.textContent).toBe("payments/holder-job");
    expect(link).toHaveAttribute("href", "/pipelines/payments/payments%2Fholder-job/run-1");
  });

  it("shows a waiting row's grouped holder", async () => {
    mockListFleetLocks.mockResolvedValue({
      items: [
        createFleetLock({
          name: "payments/smoke-lock",
          state: "held",
          jobPath: "payments/holder-job",
          runLink: "/pipelines/payments/payments%2Fholder-job/run-1",
        }),
        createFleetLock({
          name: "payments/smoke-lock",
          state: "waiting",
          jobPath: "payments/waiter-job",
        }),
      ],
    });
    renderPage();

    // The card carries no job column, so the lock name is what proves the
    // two rows landed: one held row and one waiter of the same lock.
    await waitFor(() => expect(screen.getAllByText("payments/smoke-lock")).toHaveLength(2));
    // The held row first, then its waiter.
    const rows = screen.getAllByRole("row");
    expect(within(rows[1]).getByRole("link")).toHaveAttribute(
      "href",
      "/pipelines/payments/payments%2Fholder-job/run-1",
    );
    const waiterHolder = within(rows[2]).getByRole("link", { name: /holder-job/ });
    expect(waiterHolder.textContent).toBe("payments/holder-job");
    expect(waiterHolder).toHaveAttribute("href", "/pipelines/payments/payments%2Fholder-job/run-1");
  });

  it("states that a waiting row is held elsewhere when no holder row is present", async () => {
    mockListFleetLocks.mockResolvedValue({
      items: [createFleetLock({ name: "payments/smoke-lock", state: "waiting" })],
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("held elsewhere")).toBeInTheDocument());
  });

  it("states that the tenant holds no lock and waits on none", async () => {
    mockListFleetLocks.mockResolvedValue({ items: [] });
    renderPage();

    await waitFor(() =>
      expect(screen.getByText("The tenant holds no lock and waits on none.")).toBeInTheDocument(),
    );
  });

  it("subscribes to the tenant stream with the lock event name", async () => {
    renderPage();
    await waitFor(() => expect(mockListFleetLocks).toHaveBeenCalledTimes(1));

    expect(mockUseEventStream).toHaveBeenCalledWith(
      "/api/v1/pipelines/payments/stream",
      "pipelines:payments",
      ["run", "stage", "lock"],
    );
  });

  it("polls the lock read once after 15 seconds with no stream frame", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderPage();
      await waitFor(() => expect(mockListFleetLocks).toHaveBeenCalledTimes(1));

      // No stream frame lands. The cadence alone reads the card again.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(14_000);
      });
      expect(mockListFleetLocks).toHaveBeenCalledTimes(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_000);
      });
      await waitFor(() => expect(mockListFleetLocks).toHaveBeenCalledTimes(2));

      // The poll belongs to the lock card, so the pipelines read holds still.
      expect(mockListPipelines).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("reads both lists for a lock event, and coalesces a stage event inside the window", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const { repaint } = renderPage();
      await waitFor(() => expect(mockListFleetLocks).toHaveBeenCalledTimes(1));

      // A lock frame outside the window reads both lists at once.
      mockUseEventStream.mockReturnValue({ lastEvent: frame("lock"), readyState: "closed", error: null });
      await act(async () => {
        repaint();
      });
      await waitFor(() => expect(mockListFleetLocks).toHaveBeenCalledTimes(2));
      expect(mockListPipelines).toHaveBeenCalledTimes(2);

      // A stage frame inside that window reads nothing at once: it arms one
      // timer.
      mockUseEventStream.mockReturnValue({ lastEvent: frame("stage"), readyState: "closed", error: null });
      await act(async () => {
        repaint();
      });
      expect(mockListFleetLocks).toHaveBeenCalledTimes(2);

      // The window ends, and the armed timer reads both lists once.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2100);
      });
      await waitFor(() => expect(mockListFleetLocks).toHaveBeenCalledTimes(3));
      expect(mockListPipelines).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });
});
