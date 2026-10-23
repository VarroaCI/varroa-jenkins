import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QueryClientProvider } from "@tanstack/react-query";
import { createTestQueryClient } from "../../test/render-utils";
import type { TenantCell } from "../../types";
import TenantCellCard from "./TenantCellCard";

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

/** threeMemberCell is a tenant with two active slots and one standby. */
function threeMemberCell(overrides: Partial<TenantCell> = {}): TenantCell {
  return {
    state: "placed",
    reason: "MembersApplied",
    cell: "cell-a",
    namespace: "varroa-system",
    cluster: "core",
    freeSlots: 7,
    runningNow: 2,
    otherTenantCount: 1,
    canOpenCell: false,
    members: [
      {
        cell: "cell-a", namespace: "varroa-system", cluster: "core", role: "slot 0",
        runningNow: 2, runsLast24h: 5, failedLast24h: 1, jobs: 4,
      },
      {
        cell: "cell-b", namespace: "varroa-system", cluster: "core", role: "slot 1",
        runningNow: 1, runsLast24h: 3, failedLast24h: 0, jobs: 2,
      },
      {
        cell: "cell-c", namespace: "varroa-system", cluster: "core", role: "standby",
        runningNow: 0, runsLast24h: 0, failedLast24h: 0, jobs: 0,
      },
    ],
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("TenantCellCard members", () => {
  // The card renders one entry for each member, with its role, so the
  // tenant sees every cell and not slot 0's alone.
  it("renders one card for each member of a three-member tenant", async () => {
    mockGetTenantCell.mockResolvedValue(threeMemberCell());
    renderCard();

    const list = await screen.findByRole("list", { name: "Members" });
    const entries = within(list).getAllByRole("listitem");
    expect(entries).toHaveLength(3);
    expect(within(entries[0]).getByText("cell-a")).toBeInTheDocument();
    expect(within(entries[0]).getByText("slot 0")).toBeInTheDocument();
    expect(within(entries[1]).getByText("slot 1")).toBeInTheDocument();
    expect(within(entries[2]).getByText("standby")).toBeInTheDocument();
  });

  // A standby runs no build, so its running count reads zero.
  it("shows a standby running no build", async () => {
    mockGetTenantCell.mockResolvedValue(threeMemberCell());
    renderCard();

    const list = await screen.findByRole("list", { name: "Members" });
    const standby = within(list).getAllByRole("listitem")[2];
    expect(within(standby).getByText("Running")).toBeInTheDocument();
    // Every count of a standby reads zero: it holds the folder and runs
    // no build.
    expect(within(standby).getAllByText("0")).toHaveLength(3);
  });

  // A single-member tenant carries one entry, and the top-level fields
  // describe that same cell.
  it("renders one entry for a single-member tenant", async () => {
    mockGetTenantCell.mockResolvedValue(
      threeMemberCell({
        members: [
          {
            cell: "cell-a", namespace: "varroa-system", cluster: "core", role: "slot 0",
            runningNow: 2, runsLast24h: 5, failedLast24h: 1, jobs: 4,
          },
        ],
      }),
    );
    renderCard();

    const list = await screen.findByRole("list", { name: "Members" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(1);
  });

  // A body with no member list renders no member section at all, so an
  // older answer still reads.
  it("renders no member list when the body carries none", async () => {
    mockGetTenantCell.mockResolvedValue(threeMemberCell({ members: undefined }));
    renderCard();

    await waitFor(() => expect(screen.getByText("Your cell")).toBeInTheDocument());
    expect(screen.queryByRole("list", { name: "Members" })).toBeNull();
  });
});
