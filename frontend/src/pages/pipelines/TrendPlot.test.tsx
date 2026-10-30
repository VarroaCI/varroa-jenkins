import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { TrendPlot, endLabels } from "./TrendPlot";

describe("TrendPlot", () => {
  it("places both axis labels inside the plot box", () => {
    const { container } = render(
      <TrendPlot top="3m 0s" bottom="0s" role="img" ariaLabel="Durations">
        <rect x={0} y={0} width={10} height={10} />
      </TrendPlot>,
    );
    expect(container.querySelector('[data-axis="max"]')?.textContent).toBe("3m 0s");
    expect(container.querySelector('[data-axis="min"]')?.textContent).toBe("0s");
    const svg = screen.getByRole("img", { name: "Durations" });
    expect(svg.getAttribute("preserveAspectRatio")).toBe("none");
    expect(svg.getAttribute("viewBox")).toBe("0 0 300 80");
  });

  it("centres the scale labels on the data band when inset", () => {
    const { container } = render(
      <TrendPlot top="85%" bottom="55%" inset role="group" ariaLabel="Coverage">
        <g />
      </TrendPlot>,
    );
    expect(container.querySelector('[data-axis="max"]')?.parentElement?.className).toMatch(/inset/);
  });

  it("shows the first and last build labels", () => {
    const { container } = render(
      <TrendPlot top="10" bottom="0" first="#1 · 2" last="#3 · 4" role="group" ariaLabel="Tests">
        <g />
      </TrendPlot>,
    );
    expect(container.querySelector('[data-end="first"]')?.textContent).toBe("#1 · 2");
    expect(container.querySelector('[data-end="last"]')?.textContent).toBe("#3 · 4");
  });

  it("skips slots with no value in endLabels", () => {
    const slots = [
      { point: { n: 1, v: undefined as number | undefined } },
      { point: { n: 2, v: 5 as number | undefined } },
      { point: undefined },
      { point: { n: 4, v: 7 as number | undefined } },
      { point: { n: 5, v: undefined as number | undefined } },
    ];
    const labels = endLabels(
      slots,
      (p) => `#${p.n}`,
      (p) => (p.v === undefined ? undefined : String(p.v)),
    );
    expect(labels).toEqual({ first: "#2 · 5", last: "#4 · 7" });
  });
});
