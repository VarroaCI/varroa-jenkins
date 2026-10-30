import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QualityTrendCard } from "./QualityTrendCard";
import type { JobTrendPoint, QualityTrend } from "../../types";

// window is newest first, as the API sends it.
const window: JobTrendPoint[] = [
  { runId: "run-3", buildNumber: 3, result: "SUCCESS" },
  { runId: "run-2", buildNumber: 2, result: "FAILURE" },
  { runId: "run-1", buildNumber: 1, result: "SUCCESS" },
];

function renderCard(qualityTrend: QualityTrend | undefined, win: JobTrendPoint[] = window) {
  return render(
    <MemoryRouter>
      <QualityTrendCard tenant="acme" jobPath="payments/app" qualityTrend={qualityTrend} window={win} />
    </MemoryRouter>,
  );
}

describe("QualityTrendCard", () => {
  it("renders nothing when qualityTrend is absent", () => {
    const { container } = renderCard(undefined);
    expect(container).toBeEmptyDOMElement();
  });

  it("draws a coverage series oldest first with each point linked to its run", () => {
    const { container } = renderCard({
      coverage: [
        {
          id: "jacoco",
          name: "jacoco",
          points: [
            { runId: "run-3", buildNumber: 3, linePercent: 83, branchPercent: 71 },
            { runId: "run-2", buildNumber: 2, linePercent: 82, branchPercent: 70 },
            { runId: "run-1", buildNumber: 1, linePercent: 80, branchPercent: 68 },
          ],
        },
      ],
    });
    const links = [...container.querySelectorAll("svg a")];
    expect(links.map((a) => a.getAttribute("aria-label"))).toEqual([
      "#1 · line 80.0% · branch 68.0%",
      "#2 · line 82.0% · branch 70.0%",
      "#3 · line 83.0% · branch 71.0%",
    ]);
    expect(links[0]).toHaveAttribute("href", "/pipelines/acme/payments%2Fapp/run-1");
    expect(container.querySelectorAll('polyline[data-series="line"]')).toHaveLength(1);
    expect(container.querySelectorAll('polyline[data-series="branch"]')).toHaveLength(1);
  });

  it("draws one bar per warnings point with its total", () => {
    const { container } = renderCard({
      warnings: [
        {
          id: "checkstyle",
          name: "checkstyle",
          points: [
            { runId: "run-3", buildNumber: 3, total: 12, new: 1 },
            { runId: "run-2", buildNumber: 2, total: 9 },
            { runId: "run-1", buildNumber: 1, total: 4 },
          ],
        },
      ],
    });
    const labels = [...container.querySelectorAll("svg a")].map((a) => a.getAttribute("aria-label"));
    expect(labels).toEqual(["#1 · 4 warnings", "#2 · 9 warnings", "#3 · 12 warnings · 1 new"]);
    expect(container.querySelectorAll('rect[data-series="total"]')).toHaveLength(3);
  });

  it("leaves a gap for a window run with no point", () => {
    const { container } = renderCard({
      coverage: [
        {
          id: "jacoco",
          name: "jacoco",
          points: [
            { runId: "run-3", buildNumber: 3, linePercent: 83 },
            { runId: "run-1", buildNumber: 1, linePercent: 80 },
          ],
        },
      ],
    });
    expect(container.querySelectorAll("svg a")).toHaveLength(2);
    expect(container.querySelectorAll('polyline[data-series="line"]')).toHaveLength(0);
    expect(container.querySelectorAll('circle[data-series="line"]')).toHaveLength(2);
  });

  it("omits the branch line when no point has a branch percent", () => {
    const { container } = renderCard({
      coverage: [
        {
          id: "jacoco",
          name: "jacoco",
          points: [
            { runId: "run-3", buildNumber: 3, linePercent: 83 },
            { runId: "run-2", buildNumber: 2, linePercent: 82 },
          ],
        },
      ],
    });
    expect(container.querySelectorAll('polyline[data-series="branch"]')).toHaveLength(0);
    expect(screen.queryByText("Branch")).toBeNull();
  });

  it("scales the coverage axis to the data of both series", () => {
    const { container } = renderCard({
      coverage: [
        {
          id: "jacoco",
          name: "jacoco",
          points: [
            { runId: "run-2", buildNumber: 2, linePercent: 60, branchPercent: 75 },
            { runId: "run-1", buildNumber: 1, linePercent: 80, branchPercent: 75 },
          ],
        },
      ],
    }, [window[1], window[2]]);
    expect(container.querySelector('[data-axis="max"]')).toHaveTextContent("85%");
    expect(container.querySelector('[data-axis="min"]')).toHaveTextContent("55%");
    const lineDots = container.querySelectorAll('circle[data-series="line"]');
    expect(lineDots).toHaveLength(2);
    expect(lineDots[0]).toHaveAttribute("cy", "16");
    expect(lineDots[1]).toHaveAttribute("cy", "64");
    const branchDots = container.querySelectorAll('circle[data-series="branch"]');
    expect(branchDots).toHaveLength(2);
    for (const dot of branchDots) expect(dot).toHaveAttribute("cy", "28");
    expect(container.querySelector("svg")).toHaveAttribute("aria-label", expect.stringContaining("scale 55% to 85%"));
  });

  it("centres a flat coverage series in a 10-point span", () => {
    const { container } = renderCard({
      coverage: [
        {
          id: "jacoco",
          name: "jacoco",
          points: [
            { runId: "run-3", buildNumber: 3, linePercent: 80 },
            { runId: "run-2", buildNumber: 2, linePercent: 80 },
            { runId: "run-1", buildNumber: 1, linePercent: 80 },
          ],
        },
      ],
    });
    expect(container.querySelector('[data-axis="max"]')).toHaveTextContent("85%");
    expect(container.querySelector('[data-axis="min"]')).toHaveTextContent("75%");
    const dots = container.querySelectorAll('circle[data-series="line"]');
    expect(dots).toHaveLength(3);
    for (const dot of dots) expect(dot).toHaveAttribute("cy", "40");
  });

  it("keeps a 10-point span at the top of the scale", () => {
    const { container } = renderCard({
      coverage: [
        { id: "jacoco", name: "jacoco", points: [{ runId: "run-3", buildNumber: 3, linePercent: 100 }] },
      ],
    });
    expect(container.querySelector('[data-axis="max"]')).toHaveTextContent("100%");
    expect(container.querySelector('[data-axis="min"]')).toHaveTextContent("90%");
    expect(container.querySelector('circle[data-series="line"]')).toHaveAttribute("cy", "4");
  });

  it("keeps a 10-point span at the bottom of the scale", () => {
    const { container } = renderCard({
      coverage: [
        { id: "jacoco", name: "jacoco", points: [{ runId: "run-3", buildNumber: 3, linePercent: 0 }] },
      ],
    });
    expect(container.querySelector('[data-axis="max"]')).toHaveTextContent("10%");
    expect(container.querySelector('[data-axis="min"]')).toHaveTextContent("0%");
    expect(container.querySelector('circle[data-series="line"]')).toHaveAttribute("cy", "76");
  });

  it("uses the full scale when no point has a percent", () => {
    const { container } = renderCard({
      coverage: [
        { id: "jacoco", name: "jacoco", points: [{ runId: "run-3", buildNumber: 3 }] },
      ],
    });
    expect(container.querySelector('[data-axis="max"]')).toHaveTextContent("100%");
    expect(container.querySelector('[data-axis="min"]')).toHaveTextContent("0%");
  });

  it("renders a series name as text", () => {
    const { container } = renderCard({
      warnings: [
        {
          id: "x",
          name: "<img src=x onerror=alert(1)>",
          points: [{ runId: "run-3", buildNumber: 3, total: 1 }],
        },
      ],
    });
    expect(screen.getByText("<img src=x onerror=alert(1)> · warnings")).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
  });
});
