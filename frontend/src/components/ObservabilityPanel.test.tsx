import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { ObservabilityPanel } from "./ObservabilityPanel";
import type { ControllerObservability } from "../types";

const observability: ControllerObservability = {
  sources: [
    { provider: "prometheus", status: "integrated", hints: { queryBaseURL: "http://prometheus:9090" } },
    { provider: "jenkins-api", status: "exposed" },
  ],
  capabilities: [
    "jenkins.health",
    "jenkins.jobs.summary",
    "jenkins.builds.recent",
    "jenkins.metrics.endpoint",
  ],
  level: 1,
  levelName: "Live Jenkins summary",
  warnings: [{ message: "unknown observability provider \"bad\" in annotation observability.varroa.dev/providers" }],
  freshness: { observedAt: "2026-06-08T12:00:00Z", miteTTL: 180, stale: true },
  summary: {
    totalJobs: 12,
    runningBuilds: 2,
    recentBuilds: [{ jobName: "deploy", buildNumber: 42, status: "SUCCESS" }],
  },
};

describe("ObservabilityPanel", () => {
  it("renders empty state when observability is absent", () => {
    render(<ObservabilityPanel />);
    expect(screen.getByText(/No observability data available yet/i)).toBeInTheDocument();
  });

  it("renders warnings, freshness, summary, and recent builds", () => {
    render(<ObservabilityPanel observability={observability} />);

    expect(screen.getByText(/Observability data may be stale/i)).toBeInTheDocument();
    expect(screen.getByText(/unknown observability provider/i)).toBeInTheDocument();
    expect(screen.getByText("Total jobs")).toBeInTheDocument();
    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText(/Recent Builds/i)).toBeInTheDocument();
    expect(screen.getByText(/deploy/i)).toBeInTheDocument();
    expect(screen.getByText(/Prometheus Endpoint/i)).toBeInTheDocument();
  });
});
