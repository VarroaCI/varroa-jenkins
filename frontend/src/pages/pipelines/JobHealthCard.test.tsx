import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { JobHealthCard } from "./JobHealthCard";
import type { JobHealth, JobTrendPoint } from "../../types";

const counted = (passed: number, total: number): JobHealth => ({
  bucket: passed * 100 >= total * 80 ? "sunny" : "cloudy",
  score: Math.floor((passed * 100) / total),
  window: 10,
  counted: total,
  passed,
});

// trend is newest first, as the API sends it.
const trend: JobTrendPoint[] = [
  { runId: "run-3", buildNumber: 3, result: "SUCCESS", durationMs: 180000, tests: { passed: 8, failed: 0, skipped: 1 } },
  { runId: "run-2", buildNumber: 2, result: "INTERRUPTED", durationMs: 60000 },
  { runId: "run-1", buildNumber: 1, result: "FAILURE", durationMs: 120000, tests: { passed: 5, failed: 3, skipped: 0 } },
];

function renderCard(props: { health: JobHealth; trend: JobTrendPoint[] }) {
  return render(
    <MemoryRouter>
      <JobHealthCard tenant="acme" jobPath="payments/app" health={props.health} trend={props.trend} />
    </MemoryRouter>,
  );
}

describe("JobHealthCard", () => {
  it("shows the health summary and the score", () => {
    renderCard({ health: counted(2, 3), trend });
    expect(screen.getByText(/2 of 3 counted builds passed/)).toBeInTheDocument();
    expect(screen.getByText(/· 66%/)).toBeInTheDocument();
  });

  it("lists the results oldest first, each linked to its build page", () => {
    renderCard({ health: counted(2, 3), trend });
    const strip = screen.getByRole("list", { name: "Recent results, oldest first" });
    const links = within(strip).getAllByRole("link");
    expect(links.map((l) => l.getAttribute("aria-label"))).toEqual([
      "#1 FAILURE",
      "#2 INTERRUPTED, not counted",
      "#3 SUCCESS",
    ]);
    expect(links[0]).toHaveAttribute("href", "/pipelines/acme/payments%2Fapp/run-1");
  });

  it("labels each duration bar with its build, duration, and result", () => {
    const { container } = renderCard({ health: counted(2, 3), trend });
    const bars = Array.from(container.querySelectorAll("rect > title")).map((t) => t.textContent);
    expect(bars).toEqual(["#1 · 2m 0s · FAILURE", "#2 · 1m 0s · INTERRUPTED", "#3 · 3m 0s · SUCCESS"]);
    expect(screen.getByRole("img", { name: /Build durations, oldest first\. Longest 3m 0s\./ })).toBeInTheDocument();
  });

  it("shows a legend for the result colours with Not counted", () => {
    renderCard({ health: counted(2, 3), trend });
    const legend = screen.getByRole("list", { name: "Result colours" });
    expect(within(legend).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Passed",
      "Unstable",
      "Failed",
      "Not counted",
    ]);
    expect(screen.getByText("Interrupted and not-built builds are not counted.")).toBeInTheDocument();
  });

  it("shows the duration scale and the first and last builds", () => {
    const { container } = renderCard({ health: counted(2, 3), trend });
    expect(container.querySelector('[data-axis="max"]')).toHaveTextContent("3m 0s");
    expect(container.querySelector('[data-axis="min"]')).toHaveTextContent("0s");
    expect(container.querySelector('[data-end="first"]')).toHaveTextContent("#1 · 2m 0s");
    expect(container.querySelector('[data-end="last"]')).toHaveTextContent("#3 · 3m 0s");
  });

  it("draws the test chart when a run reported suite counters", () => {
    renderCard({ health: counted(2, 3), trend });
    expect(screen.getByRole("img", { name: /Test counts per build/ })).toBeInTheDocument();
  });

  it("hides the test chart when no run reported suite counters", () => {
    renderCard({
      health: counted(1, 1),
      trend: [{ runId: "run-1", buildNumber: 1, result: "SUCCESS", durationMs: 1000 }],
    });
    expect(screen.queryByRole("img", { name: /Test counts per build/ })).not.toBeInTheDocument();
    expect(screen.queryByText("Tests")).not.toBeInTheDocument();
  });

  it("omits the duration chart when no run recorded a duration", () => {
    renderCard({
      health: counted(1, 1),
      trend: [{ runId: "run-1", buildNumber: 1, result: "SUCCESS" }],
    });
    expect(screen.queryByRole("img", { name: /Build durations/ })).not.toBeInTheDocument();
    expect(screen.queryByText("Duration")).not.toBeInTheDocument();
  });

  it("says no counted build exists for an unknown job", () => {
    renderCard({ health: { bucket: "unknown", window: 10, counted: 0, passed: 0 }, trend: [] });
    expect(screen.getByText("No counted build yet")).toBeInTheDocument();
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
  });
});
