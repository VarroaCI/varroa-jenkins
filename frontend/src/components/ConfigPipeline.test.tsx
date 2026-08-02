import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import { ConfigPipeline, type PipelineStage } from "./ConfigPipeline";

const baseStages: PipelineStage[] = [
  { label: "SOURCE", status: "—", hash: "", timestamp: "" },
  { label: "COMPOSE", status: "✓ resolved", hash: "abc123", timestamp: "" },
  { label: "DESIRE", status: "✓ converged", hash: "abc123", timestamp: "" },
  { label: "DELIVER", status: "✓ applied", hash: "abc123", timestamp: "2026-01-01T00:00:00Z", telemetry: ["Jenkins 2.440"] },
  { label: "LIVE", status: "✓ connected", hash: "", timestamp: "", telemetry: ["Mite 1.0", "last seen 12:00:00 AM"] },
];

const applyHistory = [
  {
    hash: "abc123",
    timestamp: "2026-01-01T00:00:00Z",
    succeeded: true,
    sections: [
      { name: "config", ok: true },
      { name: "rbac", ok: true },
      { name: "plugins", ok: true },
      { name: "items", ok: true },
    ],
    trigger: "reconciliation",
  },
];

describe("ConfigPipeline", () => {
  it("renders pipeline heading", () => {
    render(<ConfigPipeline stages={baseStages} />);
    expect(screen.getByText("Configuration pipeline")).toBeInTheDocument();
  });

  it("renders five stages", () => {
    render(<ConfigPipeline stages={baseStages} />);
    expect(screen.getByText("SOURCE")).toBeInTheDocument();
    expect(screen.getByText("COMPOSE")).toBeInTheDocument();
    expect(screen.getByText("DESIRE")).toBeInTheDocument();
    expect(screen.getByText("DELIVER")).toBeInTheDocument();
    expect(screen.getByText("LIVE")).toBeInTheDocument();
  });

  it("renders telemetry inside DELIVER and LIVE stage cards", () => {
    render(<ConfigPipeline stages={baseStages} />);
    expect(screen.getByText(/Jenkins 2.440/)).toBeInTheDocument();
    expect(screen.getByText(/Mite 1.0/)).toBeInTheDocument();
    expect(screen.getByText(/last seen 12:00:00 AM/)).toBeInTheDocument();
  });

  it("renders apply history", () => {
    render(<ConfigPipeline stages={baseStages} applyHistory={applyHistory} />);
    expect(screen.getByText("Apply History")).toBeInTheDocument();
  });

  it("renders guarded action buttons", () => {
    const onReload = vi.fn();
    const onReprovision = vi.fn();
    render(
      <ConfigPipeline stages={baseStages} onReload={onReload} onReprovision={onReprovision} />
    );
    expect(screen.getByText("Reload")).toBeInTheDocument();
    expect(screen.getByText("Reprovision")).toBeInTheDocument();
  });

  it("renders diff preview when diff provided", () => {
    const diff = {
      incoming: { jcasc: "jenkins:\n  systemMessage: hello", items: "", plugins: "" },
      applied: { jcasc: "jenkins:\n  systemMessage: hello", items: "", plugins: "" },
    };
    render(<ConfigPipeline stages={baseStages} diff={diff} onFetchDiff={vi.fn()} />);
    expect(screen.getByText(/systemMessage: hello/)).toBeInTheDocument();
  });

  it("shows applied-unavailable notice", () => {
    const diff = {
      incoming: { jcasc: "yaml", items: "", plugins: "" },
      appliedUnavailable: true,
    } as any;
    render(<ConfigPipeline stages={baseStages} diff={diff} onFetchDiff={vi.fn()} />);
    expect(screen.getByText(/last-applied unavailable/)).toBeInTheDocument();
  });

  it("shows paused state in DESIRE stage", () => {
    const stages = baseStages.map((s) =>
      s.label === "DESIRE" ? { ...s, status: "⏸ paused" } : s
    );
    render(<ConfigPipeline stages={stages} />);
    expect(screen.getByText(/paused/)).toBeInTheDocument();
  });

  it("shows blocked state in DESIRE stage", () => {
    const stages = baseStages.map((s) =>
      s.label === "DESIRE" ? { ...s, status: "🚫 blocked" } : s
    );
    render(<ConfigPipeline stages={stages} />);
    expect(screen.getByText(/blocked/)).toBeInTheDocument();
  });

  it("shows live drift warning", () => {
    const stages = baseStages.map((s) =>
      s.label === "LIVE" ? { ...s, status: "⚠ drift", hash: "abcdef1234567890" } : s
    );
    render(<ConfigPipeline stages={stages} />);
    expect(screen.getByText("⚠ drift")).toBeInTheDocument();
    expect(screen.getByText("abcdef123456…")).toBeInTheDocument();
  });

  it("shows failed section errors", () => {
    const stages = baseStages.map((s) =>
      s.label === "DELIVER"
        ? { ...s, status: "✗ failed", error: "config, rbac failed" }
        : s
    );
    render(<ConfigPipeline stages={stages} />);
    expect(screen.getByText("✗ failed")).toBeInTheDocument();
    expect(screen.getByText(/config, rbac failed/)).toBeInTheDocument();
  });

  it("shows running builds", () => {
    render(<ConfigPipeline stages={baseStages} runningBuilds={3} />);
    expect(screen.getByText(/3 running builds/)).toBeInTheDocument();
  });

  it("shows loading state for diff", () => {
    render(<ConfigPipeline stages={baseStages} diffLoading={true} onFetchDiff={vi.fn()} />);
    expect(screen.getByText("Loading...")).toBeInTheDocument();
  });
});
