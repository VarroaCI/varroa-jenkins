import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { QualityCard } from "./QualityCard";
import type { RunQuality } from "../../types";

function renderCard(quality?: RunQuality) {
  return render(
    <MemoryRouter>
      <QualityCard quality={quality} tenant="acme" jobPath="payments/app" />
    </MemoryRouter>,
  );
}

describe("QualityCard", () => {
  it("renders nothing when quality is absent", () => {
    const { container } = renderCard(undefined);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when quality holds no report", () => {
    const { container } = renderCard({});
    expect(container).toBeEmptyDOMElement();
  });

  it("shows a coverage row with its percents, signed delta, base build, and gate", () => {
    renderCard({
      coverage: [
        {
          id: "jacoco",
          name: "jacoco",
          line: { covered: 812, total: 1000, percent: 81.2 },
          branch: { covered: 140, total: 200, percent: 70 },
          modifiedLines: { covered: 30, total: 40, percent: 75 },
          gateStatus: "PASSED",
          delta: { baseRunId: "run-41", baseBuildNumber: 41, linePoints: 1.4, branchPoints: -0.5 },
        },
      ],
    });
    expect(screen.getByText("81.2%")).toBeInTheDocument();
    expect(screen.getByText("70.0%")).toBeInTheDocument();
    expect(screen.getByText("75.0%")).toBeInTheDocument();
    expect(screen.getByText(/line \+1\.4 pts, branch -0\.5 pts/)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "#41" });
    expect(link).toHaveAttribute("href", "/pipelines/acme/payments%2Fapp/run-41");
    expect(screen.getByText("PASSED")).toBeInTheDocument();
  });

  it("shows a dash for a counter with no percent and no delta", () => {
    renderCard({
      coverage: [{ id: "jacoco", name: "jacoco", line: { covered: 0, total: 0 } }],
    });
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByText(/vs/)).toBeNull();
  });

  it("names an earlier build when the delta has no build number", () => {
    renderCard({
      coverage: [
        {
          id: "jacoco",
          name: "jacoco",
          line: { covered: 1, total: 2, percent: 50 },
          delta: { baseRunId: "run-7", linePoints: 0 },
        },
      ],
    });
    expect(screen.getByRole("link", { name: "an earlier build" })).toHaveAttribute(
      "href",
      "/pipelines/acme/payments%2Fapp/run-7",
    );
  });

  it("shows new and fixed counts on a warnings row that has them", () => {
    renderCard({
      warnings: [
        {
          id: "w1",
          name: "checkstyle",
          total: 12,
          error: 0,
          high: 2,
          normal: 7,
          low: 3,
          new: 1,
          fixed: 4,
          gateStatus: "WARNING",
          delta: { baseRunId: "run-41", baseBuildNumber: 41, total: -3 },
        },
      ],
    });
    expect(screen.getByRole("columnheader", { name: "New" })).toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: "Fixed" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "1" })).toBeInTheDocument();
    expect(screen.getByRole("cell", { name: "4" })).toBeInTheDocument();
    expect(screen.getByText(/-3 vs/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "#41" })).toBeInTheDocument();
    expect(screen.getByText("WARNING")).toBeInTheDocument();
  });

  it("omits the new and fixed columns when no warnings row has them", () => {
    renderCard({
      warnings: [{ id: "w1", name: "checkstyle", total: 3, error: 0, high: 0, normal: 3, low: 0 }],
    });
    expect(screen.queryByRole("columnheader", { name: "New" })).toBeNull();
    expect(screen.queryByRole("columnheader", { name: "Fixed" })).toBeNull();
  });

  it("renders a report name as text", () => {
    const { container } = renderCard({
      coverage: [{ id: "x", name: "<img src=x onerror=alert(1)>", line: { covered: 1, total: 1, percent: 100 } }],
    });
    expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
  });
});
