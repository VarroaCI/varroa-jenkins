import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import type { TeamCell } from "../../types";
import TeamCellCard, { CELL_STATE_LABELS } from "./TeamCellCard";
import styles from "./TeamCellCard.module.css";

const mockGetTeamCell = vi.fn();
vi.mock("../../api/client", () => ({
  getTeamCell: (...args: unknown[]) => mockGetTeamCell(...args),
}));

function renderCard() {
  const queryClient = createTestQueryClient();
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>
        <TeamCellCard team="acme" />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

/** placedCell is a full answer. Each test overrides the fields it reads. */
function placedCell(overrides: Partial<TeamCell> = {}): TeamCell {
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
  mockGetTeamCell.mockReset();
});

describe("TeamCellCard", () => {
  it("shows the cell of a placed tenant", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell());
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
    mockGetTeamCell.mockResolvedValue({
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
    mockGetTeamCell.mockResolvedValue({ state: "unplaced", reason: "NoFreeCell", canOpenCell: false });
    renderCard();

    expect(await screen.findByText("Not placed")).toBeInTheDocument();
    expect(screen.getByText("NoFreeCell")).toBeInTheDocument();
    expect(
      screen.getByText(
        "This team has no cell yet. The reason above names the cause. Ask an operator to act on it.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText("Free slots")).not.toBeInTheDocument();
    expect(screen.queryByText("Queue")).not.toBeInTheDocument();
    expect(screen.queryByText("Cell details")).not.toBeInTheDocument();
    expect(screen.queryByText(/^Updated /)).not.toBeInTheDocument();
  });

  it("explains a move", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ state: "moving", reason: "TenantMoving" }));
    renderCard();

    expect(await screen.findByText("Moving")).toBeInTheDocument();
    expect(screen.getByText("TenantMoving")).toBeInTheDocument();
    expect(
      screen.getByText("Varroa is moving this team to another cell. Builds wait until the move ends."),
    ).toBeInTheDocument();
  });

  it("explains a rebuild", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ state: "rehydrating", reason: "TenantPlaced" }));
    renderCard();

    expect(await screen.findByText("Rehydrating")).toBeInTheDocument();
    expect(screen.getByText("TenantPlaced")).toBeInTheDocument();
    expect(
      screen.getByText("The cell is rebuilding this team's folders. Builds start when the rebuild ends."),
    ).toBeInTheDocument();
  });

  it("renders nothing for a team on its own controller", async () => {
    mockGetTeamCell.mockResolvedValue({ state: "notCellHosted", canOpenCell: false });
    const { container } = renderCard();

    await waitFor(() => expect(mockGetTeamCell).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByText("Loading cell…")).not.toBeInTheDocument());
    expect(screen.queryByText("Your cell")).not.toBeInTheDocument();
    expect(container).toBeEmptyDOMElement();
  });

  it("renders the whole card and no link for a caller without controller access", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ canOpenCell: false }));
    renderCard();

    expect(await screen.findByText("cell-a")).toBeInTheDocument();
    expect(screen.getByText("Queue")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Cell details" })).not.toBeInTheDocument();
  });

  it("links an operator to the cell's controller page", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ canOpenCell: true }));
    renderCard();

    const link = await screen.findByRole("link", { name: "Cell details" });
    expect(link).toHaveAttribute("href", "/controllers/core/varroa-system/cell-a?tab=cell");
  });

  it("renders no sharing line when the count is absent", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ otherTenantCount: undefined }));
    renderCard();

    await screen.findByText("cell-a");
    expect(screen.queryByText(/shares? this cell\./)).not.toBeInTheDocument();
  });

  it("names a cell of its own", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ otherTenantCount: 0 }));
    renderCard();

    expect(await screen.findByText("No other tenant shares this cell.")).toBeInTheDocument();
  });

  it("counts one neighbour in the singular", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ otherTenantCount: 1 }));
    renderCard();

    expect(await screen.findByText("1 other tenant shares this cell.")).toBeInTheDocument();
  });

  it("reports an absent queue length as not reported", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ queueLength: undefined }));
    renderCard();

    await screen.findByText("cell-a");
    expect(screen.getByText("Queue").nextSibling).toHaveTextContent("not reported");
    expect(screen.getByText("Queue").nextSibling).not.toHaveTextContent("0");
  });

  it("drops the pool row when the response omits the pool", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ cellPool: undefined }));
    renderCard();

    await screen.findByText("cell-a");
    expect(screen.queryByText("Pool")).not.toBeInTheDocument();
    expect(screen.getByText("Profile")).toBeInTheDocument();
  });

  it("hides the card when the read fails", async () => {
    mockGetTeamCell.mockRejectedValue(new Error("boom"));
    const { container } = renderCard();

    await waitFor(() => expect(container).toBeEmptyDOMElement());
    expect(screen.queryByText("Your cell")).not.toBeInTheDocument();
  });

  it("keeps the landmark while the read is pending", async () => {
    mockGetTeamCell.mockReturnValue(new Promise(() => {}));
    renderCard();

    expect(await screen.findByRole("region", { name: "Your cell" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { level: 2, name: "Your cell" })).toBeInTheDocument();
    expect(screen.getByText("Loading cell…")).toBeInTheDocument();
  });

  it("is a region named Your cell, and announces a moving state in text", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell({ state: "moving" }));
    renderCard();

    expect(await screen.findByText("Moving")).toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Your cell" })).toBeInTheDocument();
  });

  it("states when the browser last read the record", async () => {
    mockGetTeamCell.mockResolvedValue(placedCell());
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
      mockGetTeamCell.mockResolvedValue(placedCell({ state, reason: "TenantPlaced" }));
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
