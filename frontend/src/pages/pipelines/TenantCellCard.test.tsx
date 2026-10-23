import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import type { TenantCell } from "../../types";
import TenantCellCard, { CELL_STATE_LABELS } from "./TenantCellCard";
import styles from "./TenantCellCard.module.css";

const mockGetTenantCell = vi.fn();
vi.mock("../../api/client", () => ({
  getTenantCell: (...args: unknown[]) => mockGetTenantCell(...args),
}));

function renderCard() {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <TenantCellCard tenant="acme" />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

/** placedCell is a full answer. Each test overrides the fields it reads. */
function placedCell(overrides: Partial<TenantCell> = {}): TenantCell {
  return {
    state: "placed",
    reason: "TenantPlaced",
    cell: "cell-a",
    namespace: "varroa-system",
    cluster: "core",
    cellPool: "pool-lts",
    profile: "lts-2.462",
    jenkinsVersion: "2.462.3",
    freeSlots: 7,
    queueLength: 3,
    runningNow: 2,
    otherTenantCount: 2,
    canOpenCell: false,
    ...overrides,
  };
}

beforeEach(() => {
  mockGetTenantCell.mockReset();
});

describe("TenantCellCard", () => {
  it("shows the cell of a placed tenant", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell());
    renderCard();

    expect(await screen.findByText("Placed")).toBeInTheDocument();
    expect(screen.getByText("cell-a")).toBeInTheDocument();
    expect(screen.getByText("pool-lts")).toBeInTheDocument();
    expect(screen.getByText("lts-2.462")).toBeInTheDocument();
    expect(screen.getByText("2.462.3")).toBeInTheDocument();
    expect(screen.getByText("Free slots").nextSibling).toHaveTextContent("7");
    expect(screen.getByText("Queue").nextSibling).toHaveTextContent("3");
    expect(screen.getByText("Your running builds").nextSibling).toHaveTextContent("2");
    expect(screen.getByText("2 other tenants share this cell.")).toBeInTheDocument();
  });

  it("names no other tenant of the cell", async () => {
    // The extra keys stand for data the tenant gate refuses. The card reads
    // the declared fields only, so neither name can reach the page.
    mockGetTenantCell.mockResolvedValue({
      ...placedCell(),
      tenants: ["billing", "payments"],
      tenantRunningNow: { billing: 4 },
    });
    const { container } = renderCard();

    await screen.findByText("Placed");
    expect(container.textContent).not.toContain("billing");
    expect(container.textContent).not.toContain("payments");
    expect(screen.getByText("2 other tenants share this cell.")).toBeInTheDocument();
  });

  it("shows the reason and the operator sentence of an unplaced tenant", async () => {
    mockGetTenantCell.mockResolvedValue({ state: "unplaced", reason: "NoFreeCell", canOpenCell: false });
    renderCard();

    expect(await screen.findByText("Not placed")).toBeInTheDocument();
    expect(screen.getByText("NoFreeCell")).toBeInTheDocument();
    expect(
      screen.getByText(
        "This tenant has no cell yet. The reason above names the cause. Ask an operator to act on it.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("Free slots")).not.toBeInTheDocument();
    expect(screen.queryByText("Queue")).not.toBeInTheDocument();
    expect(screen.queryByText("Cell details")).not.toBeInTheDocument();
    expect(screen.queryByText(/^Updated /)).not.toBeInTheDocument();
  });

  it("renders the tenant warning", async () => {
    mockGetTenantCell.mockResolvedValue(
      placedCell({
        warning: {
          reason: "ItemsApplyFailed",
          message: "create acme/repos: HTTP 500: Jenkins internal error",
        },
      }),
    );
    renderCard();

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Warning");
    expect(alert).toHaveTextContent("ItemsApplyFailed");
    expect(alert).toHaveTextContent("create acme/repos: HTTP 500: Jenkins internal error");
  });

  it("renders no warning block without a warning", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell());
    renderCard();

    expect(await screen.findByText("Placed")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("explains a move", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ state: "moving", reason: "TenantMoving" }));
    renderCard();

    expect(await screen.findByText("Moving")).toBeInTheDocument();
    expect(screen.getByText("TenantMoving")).toBeInTheDocument();
    expect(
      screen.getByText("Varroa is moving this tenant to another cell. Builds wait until the move ends."),
    ).toBeInTheDocument();
  });

  it("explains a rebuild", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ state: "rehydrating", reason: "TenantPlaced" }));
    renderCard();

    expect(await screen.findByText("Rehydrating")).toBeInTheDocument();
    expect(screen.getByText("TenantPlaced")).toBeInTheDocument();
    expect(
      screen.getByText("The cell is rebuilding this tenant's folders. Builds start when the rebuild ends."),
    ).toBeInTheDocument();
  });

  it("renders nothing for a tenant on its own controller", async () => {
    mockGetTenantCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    const { container } = renderCard();

    await waitFor(() => expect(mockGetTenantCell).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByText("Loading cell…")).not.toBeInTheDocument());
    expect(screen.queryByText("Your cell")).not.toBeInTheDocument();
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the whole card and no link for a caller without controller access", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ canOpenCell: false }));
    renderCard();

    expect(await screen.findByText("cell-a")).toBeInTheDocument();
    expect(screen.getByText("Queue")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Cell details" })).not.toBeInTheDocument();
  });

  it("links an operator to the cell's controller page", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ canOpenCell: true }));
    renderCard();

    const link = await screen.findByRole("link", { name: "Cell details" });
    expect(link).toHaveAttribute("href", "/controllers/core/varroa-system/cell-a?tab=cell");
  });

  it("renders no sharing line when the count is absent", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ otherTenantCount: undefined }));
    renderCard();

    await screen.findByText("cell-a");
    expect(screen.queryByText(/shares? this cell\./)).not.toBeInTheDocument();
  });

  it("names a cell of its own", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ otherTenantCount: 0 }));
    renderCard();

    expect(await screen.findByText("No other tenant shares this cell.")).toBeInTheDocument();
  });

  it("counts one neighbour in the singular", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ otherTenantCount: 1 }));
    renderCard();

    expect(await screen.findByText("1 other tenant shares this cell.")).toBeInTheDocument();
  });

  it("reports an absent queue length as not reported", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ queueLength: undefined }));
    renderCard();

    await screen.findByText("cell-a");
    expect(screen.getByText("Queue").nextSibling).toHaveTextContent("not reported");
    expect(screen.getByText("Queue").nextSibling).not.toHaveTextContent("0");
  });

  it("drops the pool row when the response omits the pool", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ cellPool: undefined }));
    renderCard();

    await screen.findByText("cell-a");
    expect(screen.queryByText("Pool")).not.toBeInTheDocument();
    expect(screen.getByText("Profile")).toBeInTheDocument();
  });

  it("hides the card when the read fails", async () => {
    mockGetTenantCell.mockRejectedValue(new Error("boom"));
    const { container } = renderCard();

    await waitFor(() => expect(container).toBeEmptyDOMElement());
    expect(screen.queryByText("Your cell")).not.toBeInTheDocument();
  });

  it("keeps the landmark while the read is pending", async () => {
    mockGetTenantCell.mockReturnValue(new Promise(() => {}));
    renderCard();

    expect(await screen.findByRole("region", { name: "Your cell" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "Your cell" })).toBeInTheDocument();
    expect(screen.getByText("Loading cell…")).toBeInTheDocument();
  });

  it("is a region named Your cell, and announces a moving state in text", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell({ state: "moving" }));
    renderCard();

    expect(await screen.findByText("Moving")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Your cell" })).toBeInTheDocument();
  });

  it("states when the browser last read the record", async () => {
    mockGetTenantCell.mockResolvedValue(placedCell());
    renderCard();

    const updated = await screen.findByText(/^Updated /);
    expect(updated).toHaveTextContent("Updated just now");
    expect(updated.textContent).not.toContain("heartbeat");
  });

  it("shows the CELL_STATE_LABELS text as the badge of every state", async () => {
    const cases = [
      ["placed", styles.badgeOk],
      ["moving", styles.badgeInfo],
      ["rehydrating", styles.badgeInfo],
      ["unplaced", styles.badgeWarn],
    ] as const;

    for (const [state, badgeClass] of cases) {
      mockGetTenantCell.mockResolvedValue(placedCell({ state, reason: "TenantPlaced" }));
      const { container, unmount } = renderCard();

      // The badge is the span the card's own badge row holds, so its class
      // names the element and its text proves the label comes from
      // CELL_STATE_LABELS.
      await waitFor(() =>
        expect(container.querySelector(`.${styles.badgeRow} > span`)).not.toBeNull(),
      );
      const badge = container.querySelector(`.${styles.badgeRow} > span`) as HTMLElement;

      expect(badge).toHaveClass(badgeClass);
      expect(badge.textContent).toBe(CELL_STATE_LABELS[state]);
      unmount();
    }
  });
});
