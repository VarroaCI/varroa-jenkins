import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../test/render-utils";
import { ApiError } from "../hooks/useApi";
import { createFleetLock } from "../test/factories";
import Locks from "./Locks";

const mockListAllFleetLocks = vi.fn();
const mockReleaseFleetLock = vi.fn();
vi.mock("../api/client", () => ({
  listAllFleetLocks: (...args: unknown[]) => mockListAllFleetLocks(...args),
  releaseFleetLock: (...args: unknown[]) => mockReleaseFleetLock(...args),
}));

/** The permissions the page reads. The default grants read only. */
let mockPermissions: unknown = { global: { fleetlocks: { read: true } }, scopes: [] };
vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ permissions: mockPermissions }),
}));

const RELEASE_PERMISSIONS = { global: { fleetlocks: { read: true, release: true } }, scopes: [] };

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
  mockReleaseFleetLock.mockReset();
  mockPermissions = { global: { fleetlocks: { read: true } }, scopes: [] };
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

  it("renders the cluster column", async () => {
    mockListAllFleetLocks.mockResolvedValue({
      items: [createFleetLock({ name: "payments/smoke-lock", cluster: "member-a", state: "held" })],
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("Cluster")).toBeInTheDocument());
    const row = screen.getByText("payments/smoke-lock").closest("tr") as HTMLElement;
    expect(within(row).getByText("member-a")).toBeInTheDocument();
  });

  it("groups a waiter with a holder on another cluster", async () => {
    mockListAllFleetLocks.mockResolvedValue({
      items: [
        createFleetLock({
          name: "payments/smoke-lock",
          cluster: "member-a",
          state: "held",
          jobPath: "payments/holder-job",
        }),
        createFleetLock({
          name: "payments/smoke-lock",
          cluster: "core",
          state: "waiting",
          jobPath: "payments/waiter-job",
        }),
      ],
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("payments/waiter-job")).toBeInTheDocument());
    expect(screen.queryByText("held elsewhere")).not.toBeInTheDocument();
    const waiter = screen.getByText("payments/waiter-job").closest("tr") as HTMLElement;
    expect(within(waiter).getByText("core")).toBeInTheDocument();
    expect(within(waiter).getByText("payments/holder-job")).toBeInTheDocument();
  });

  it("polls the read and opens no stream", async () => {
    mockListAllFleetLocks.mockResolvedValue({ items: [] });
    renderPage();

    await waitFor(() => expect(mockListAllFleetLocks).toHaveBeenCalledTimes(1));
    expect(eventSourceSpy).not.toHaveBeenCalled();
  });

  describe("force release", () => {
    function seedReleasing() {
      mockListAllFleetLocks.mockResolvedValue({
        items: [
          createFleetLock({ name: "payments/deploy", state: "releasing", runId: "run-7", tenant: "payments" }),
          createFleetLock({ name: "payments/smoke", state: "held" }),
        ],
      });
    }

    async function openDialog() {
      fireEvent.click(await screen.findByRole("button", { name: "Force release" }));
      return screen.getByRole("dialog");
    }

    it("shows the action only on a releasing row for a caller with fleetlocks:release", async () => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      renderPage();

      const buttons = await screen.findAllByRole("button", { name: "Force release" });
      expect(buttons).toHaveLength(1);
      const row = screen.getByText("payments/deploy").closest("tr") as HTMLElement;
      expect(within(row).getByRole("button", { name: "Force release" })).toBeInTheDocument();
    });

    it("hides the action from a caller without the release verb", async () => {
      seedReleasing();
      renderPage();

      await waitFor(() => expect(screen.getByText("payments/deploy")).toBeInTheDocument());
      expect(screen.queryByRole("button", { name: "Force release" })).not.toBeInTheDocument();
    });

    it("requires a reason and states the no-cell rule", async () => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      renderPage();

      const dialog = await openDialog();
      expect(within(dialog).getByText(/runs on no cell/)).toBeInTheDocument();
      const confirm = within(dialog).getByRole("button", { name: "Force release" });
      expect(confirm).toBeDisabled();
      fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "   " } });
      expect(confirm).toBeDisabled();
      expect(mockReleaseFleetLock).not.toHaveBeenCalled();
    });

    it("posts name, runId, and reason, closes, and reads the list again", async () => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      mockReleaseFleetLock.mockResolvedValue({
        name: "payments/deploy", runId: "run-7", cluster: "core", tenant: "payments",
        jobPath: "payments/app-a", state: "released",
      });
      renderPage();

      const dialog = await openDialog();
      fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: " pod is gone " } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Force release" }));

      await waitFor(() =>
        expect(mockReleaseFleetLock).toHaveBeenCalledWith({
          name: "payments/deploy", runId: "run-7", reason: "pod is gone",
        }),
      );
      await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
      await waitFor(() => expect(mockListAllFleetLocks).toHaveBeenCalledTimes(2));
    });

    it.each([
      [409, "cell cells/cell-a on cluster core still runs the holder's pod c-1."],
      [409, "the lock is held; only a releasing lock can be force-released"],
      [404, "fleet lock not found"],
      [503, "the record store did not confirm the release; read the lock again before you retry"],
    ])("shows the %i status and the server message", async (status, message) => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      mockReleaseFleetLock.mockRejectedValue(new ApiError(status, "fixed copy", { error: message }));
      renderPage();

      const dialog = await openDialog();
      fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "stuck" } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Force release" }));

      const alert = await within(dialog).findByRole("alert");
      expect(alert).toHaveTextContent(`${status}: ${message}`);
      expect(screen.getByRole("dialog")).toBeInTheDocument();
      await waitFor(() => expect(mockListAllFleetLocks).toHaveBeenCalledTimes(2));
    });

    it("links the holder cell when a live pod still runs the holder", async () => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      mockReleaseFleetLock.mockRejectedValue(new ApiError(409, "fixed copy", {
        error: "cell cells/cell-a on cluster core still runs the holder's pod c-1.",
        holderCell: { cluster: "core", namespace: "cells", name: "cell-a" },
      }));
      renderPage();

      const dialog = await openDialog();
      fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "stuck" } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Force release" }));

      const link = await within(dialog).findByRole("link", { name: "cells/cell-a" });
      expect(link).toHaveAttribute("href", "/controllers/core/cells/cell-a");
      expect(within(dialog).getByText(/when its Jenkins does not run/)).toBeInTheDocument();
    });

    it("shows no cell link for a refusal with no holder cell", async () => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      mockReleaseFleetLock.mockRejectedValue(new ApiError(409, "fixed copy", {
        error: "the lock is held; only a releasing lock can be force-released",
      }));
      renderPage();

      const dialog = await openDialog();
      fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "stuck" } });
      fireEvent.click(within(dialog).getByRole("button", { name: "Force release" }));

      await within(dialog).findByRole("alert");
      expect(within(dialog).queryByRole("link")).not.toBeInTheDocument();
    });

    it("focuses the reason and closes on Escape without a request", async () => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      renderPage();

      const dialog = await openDialog();
      expect(within(dialog).getByLabelText("Reason")).toHaveFocus();
      fireEvent.keyDown(window, { key: "Escape" });
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(mockReleaseFleetLock).not.toHaveBeenCalled();
    });

    it("closes on Cancel without a request", async () => {
      mockPermissions = RELEASE_PERMISSIONS;
      seedReleasing();
      renderPage();

      const dialog = await openDialog();
      fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(mockReleaseFleetLock).not.toHaveBeenCalled();
    });
  });
});
