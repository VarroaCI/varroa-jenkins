import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { PipelinesOverview } from "../../types";
import { FleetCounters } from "./FleetCounters";

function overview(totals: Partial<PipelinesOverview["totals"]>): PipelinesOverview {
  return {
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
      ...totals,
    },
    attention: { failed24h: 0, waitingInput: 0, awaitingCell: 0, locksHeld: 0 },
    tenants: [],
  };
}

describe("FleetCounters", () => {
  it("counters show the run delta and the pass rate", () => {
    render(
      <FleetCounters
        overview={overview({ runs24h: 12, runsPrior24h: 9, passRate: 0.75, passed: 3, failed: 1, interrupted: 0, finished: 4 })}
      />,
    );
    expect(screen.getByText(/\+3 vs prior 24 h/)).toBeInTheDocument();
    expect(screen.getByText("75%")).toBeInTheDocument();
  });

  it("shows a dash for a pass rate with no finished run", () => {
    render(<FleetCounters overview={overview({ runs24h: 0, finished: 0 })} />);
    const tile = screen.getByText("Pass rate").parentElement?.parentElement;
    expect(tile).toBeTruthy();
    expect(within(tile as HTMLElement).getByText("—")).toBeInTheDocument();
  });

  it("shows the running count with the queued count", () => {
    render(<FleetCounters overview={overview({ running: 2, queued: 5, medianQueueWaitMs: 90_000 })} />);
    expect(screen.getByText(/5 queued · median wait 1m 30s/)).toBeInTheDocument();
  });

  it("shows the median duration with the p90", () => {
    render(<FleetCounters overview={overview({ medianDurationMs: 60_000, p90DurationMs: 180_000 })} />);
    expect(screen.getByText("p90 3m 0s")).toBeInTheDocument();
  });
});
