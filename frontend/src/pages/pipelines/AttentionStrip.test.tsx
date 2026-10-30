import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import type { PipelinesOverview } from "../../types";
import { AttentionStrip } from "./AttentionStrip";

function overview(over: Partial<PipelinesOverview> = {}, attention: Partial<PipelinesOverview["attention"]> = {}) {
  const base: PipelinesOverview = {
    windowStart: "2026-10-29T12:00:00Z",
    localCluster: "core",
    clusters: 1,
    canReadFleetLocks: false,
    totals: {
      runs24h: 0,
      runsPrior24h: 0,
      passed: 0,
      failed: 0,
      interrupted: 0,
      finished: 0,
      running: 0,
      queued: 0,
    },
    attention: { failed24h: 0, waitingInput: 0, awaitingCell: 0, locksHeld: 0, ...attention },
    tenants: [],
  };
  return { ...base, ...over };
}

function renderStrip(o: PipelinesOverview, onPreset = vi.fn()) {
  render(
    <MemoryRouter>
      <AttentionStrip overview={o} onPreset={onPreset} />
    </MemoryRouter>,
  );
  return onPreset;
}

describe("AttentionStrip", () => {
  it("renders only non-zero chips in order", () => {
    renderStrip(
      overview(
        {
          tenants: [
            {
              name: "idle",
              clusters: ["core"],
              placement: "awaitingCell",
              runs24h: 0,
              failed24h: 0,
              running: 0,
              waitingInput: 0,
              locksHeld: 0,
              recent: [],
            },
          ],
        },
        { failed24h: 3, waitingInput: 0, awaitingCell: 1, locksHeld: 2 },
      ),
    );
    const chips = screen.getAllByRole("listitem").map((li) => li.textContent);
    expect(chips).toEqual(["3 failed in the last 24 h", "1 tenant awaiting a cell", "2 fleet locks held"]);
  });

  it("the failed chip sets the preset", async () => {
    const user = userEvent.setup();
    const onPreset = renderStrip(overview({}, { failed24h: 3 }));
    await user.click(screen.getByRole("button", { name: "3 failed in the last 24 h" }));
    expect(onPreset).toHaveBeenCalledWith({ result: "FAILURE", since: "24h" });
  });

  it("the waiting chip sets the status preset", async () => {
    const user = userEvent.setup();
    const onPreset = renderStrip(overview({}, { waitingInput: 1 }));
    await user.click(screen.getByRole("button", { name: "1 run waits for input" }));
    expect(onPreset).toHaveBeenCalledWith({ status: "WAITING_INPUT" });
  });

  it("all zero shows Nothing needs attention.", () => {
    renderStrip(overview());
    expect(screen.getByText("Nothing needs attention.")).toBeInTheDocument();
    expect(screen.queryByRole("listitem")).not.toBeInTheDocument();
  });

  it("lock chip links to /locks for a lock reader", () => {
    renderStrip(overview({ canReadFleetLocks: true }, { locksHeld: 2 }));
    const strip = screen.getByRole("list", { name: "Needs attention" });
    expect(within(strip).getByRole("link", { name: "2 fleet locks held" })).toHaveAttribute("href", "/locks");
  });

  it("lock chip links to the first tenant that holds a lock for a non-reader", () => {
    renderStrip(
      overview(
        {
          tenants: [
            { name: "ops", clusters: ["core"], placement: "placed", runs24h: 0, failed24h: 0, running: 0, waitingInput: 0, locksHeld: 0, recent: [] },
            { name: "pay", clusters: ["core"], placement: "placed", runs24h: 0, failed24h: 0, running: 0, waitingInput: 0, locksHeld: 1, recent: [] },
          ],
        },
        { locksHeld: 1 },
      ),
    );
    expect(screen.getByRole("link", { name: "1 fleet lock held" })).toHaveAttribute("href", "/pipelines/pay");
  });
});
