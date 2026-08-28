import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { buildDeliveryNodes, DeliveryPipeline, isAsleep } from "./DeliveryPipeline";
import type { ControllerDetail } from "../hooks/useControllers";

function baseCtrl(overrides: Record<string, any> = {}): ControllerDetail {
  return {
    name: "my-ctrl",
    namespace: "my-ns",
    cluster: "core",
    phase: "Connected",
    endpoint: "https://builds.example.com",
    version: "2.555.3",
    jenkinsVersion: "2.555.3",
    jenkinsHealth: "healthy",
    miteConnected: true,
    miteVersion: "0.1.0",
    lastSeen: "2026-01-01T00:00:00Z",
    appliedBundleHash: "31ae2452abcdef",
    effectiveBundle: { name: "getting-started-bundle", namespace: "my-ns", builtIn: false },
    composedBundleRef: { name: "getting-started-bundle" },
    desiredStateHash: "1ada258a",
    lastApplyResult: {
      hash: "1ada258a",
      timestamp: "2026-01-01T00:00:00Z",
      succeeded: true,
      sections: [
        { name: "config", ok: true },
        { name: "rbac", ok: true },
        { name: "plugins", ok: true },
        { name: "items", ok: true },
      ],
    },
    observability: { summary: { totalJobs: 4, runningBuilds: 0 } },
    ...overrides,
  } as ControllerDetail;
}

const NOW = Date.parse("2026-07-17T14:25:00Z");

describe("buildDeliveryNodes", () => {
  it("connected-healthy: ok lamps, applied line, live jenkins, flow rails", () => {
    const model = buildDeliveryNodes(baseCtrl(), NOW);
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;

    const [bundle, apply, jenkins] = model.nodes;
    expect(bundle.name).toBe("Bundle");
    expect(bundle.line).toBe("getting-started-bundle");
    expect(bundle.lamp).toBe("ok");
    expect(bundle.sub).toEqual([{ t: "composed · " }, { t: "31ae2452", mono: true }]);

    expect(apply.lamp).toBe("ok");
    expect(apply.line).toMatch(/^applied /);
    expect(apply.checks?.every((c) => c.state === "ok")).toBe(true);

    expect(jenkins.lamp).toBe("live");
    expect(jenkins.line).toBe("2.555.3 · healthy");
    expect(jenkins.sub?.[0].t).toBe("mite 0.1.0 · heartbeat ");

    expect(model.rails).toEqual({ bundleToApply: "flow", applyToJenkins: "flow" });
  });

  it("apply-failed: bad lamp, failed chip, error text, off rail to jenkins", () => {
    const model = buildDeliveryNodes(
      baseCtrl({
        lastApplyResult: {
          hash: "1ada258a",
          timestamp: "2026-01-01T00:00:00Z",
          succeeded: false,
          sections: [
            { name: "config", ok: true },
            { name: "rbac", ok: false, error: "denied" },
          ],
        },
      }),
      NOW,
    );
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;

    const apply = model.nodes[1];
    expect(apply.lamp).toBe("bad");
    expect(apply.line).toBe("rbac rejected");
    expect(apply.error).toBe("denied");
    expect(apply.checks).toEqual([
      { label: "config", state: "ok" },
      { label: "rbac", state: "bad" },
    ]);
    expect(model.rails.bundleToApply).toBe("off");
    // jenkins is still live (previous config) → rail from apply is off
    expect(model.nodes[2].lamp).toBe("live");
  });

  it("hash mismatch + failed apply in automatic mode: checks show real ok/bad, not run", () => {
    // The lamp/line resolve to "rejected" (succeeded === false takes
    // precedence), so the in-flight "run" interpretation must not leak into
    // the per-section checks: they should mirror the real ok/bad outcome.
    const model = buildDeliveryNodes(
      baseCtrl({
        desiredStateHash: "8f31bc07",
        lastApplyResult: {
          hash: "1ada258a",
          timestamp: "2026-01-01T00:00:00Z",
          succeeded: false,
          sections: [
            { name: "config", ok: true },
            { name: "rbac", ok: false, error: "denied" },
          ],
        },
      }),
      NOW,
    );
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;

    const apply = model.nodes[1];
    // Still the rejected outcome up top (hash mismatch + failure).
    expect(apply.lamp).toBe("bad");
    expect(apply.line).toBe("rbac rejected");
    expect(apply.error).toBe("denied");
    // And the checks reflect each section, not the in-flight "run" state.
    expect(apply.checks).toEqual([
      { label: "config", state: "ok" },
      { label: "rbac", state: "bad" },
    ]);
    expect(apply.checks?.some((c) => c.state === "run")).toBe(false);
  });

  it("applying: warn lamp, busy rails, running checks", () => {
    const model = buildDeliveryNodes(
      baseCtrl({ desiredStateHash: "8f31bc07", lastApplyResult: { hash: "1ada258a", timestamp: "2026-01-01T00:00:00Z", succeeded: true, sections: [{ name: "config", ok: true }, { name: "rbac", ok: true }] } }),
      NOW,
    );
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;

    const apply = model.nodes[1];
    expect(apply.lamp).toBe("warn");
    expect(apply.line).toBe("applying new configuration…");
    expect(apply.checks?.every((c) => c.state === "run")).toBe(true);
    expect(model.rails).toEqual({ bundleToApply: "busy", applyToJenkins: "busy" });
  });

  it("hibernated short-circuits to the quiet model", () => {
    const model = buildDeliveryNodes(baseCtrl({ hibernated: true, phase: "Hibernated" }), NOW);
    expect(model.kind).toBe("quiet");
    if (model.kind === "quiet") {
      expect(model.powerLabel).toBe("Hibernated");
      expect(model.hash).toBe("1ada258a");
      expect(model.jobs).toBe(4);
    }
    expect(isAsleep(baseCtrl({ hibernated: true }))).toBe(true);
    expect(isAsleep(baseCtrl({ phase: "Hibernated" }))).toBe(true);
    expect(isAsleep(baseCtrl({ phase: "Stopped" }))).toBe(true);
    expect(isAsleep(baseCtrl())).toBe(false);
  });

  it("stopped quiet model uses the Stopped label", () => {
    const model = buildDeliveryNodes(baseCtrl({ powerState: "Stopped", phase: "Stopped" }), NOW);
    expect(model.kind).toBe("quiet");
    if (model.kind === "quiet") expect(model.powerLabel).toBe("Stopped");
  });

  it("waking Provisioning: Provisioning now, Running/Connected todo", () => {
    const model = buildDeliveryNodes(baseCtrl({ phase: "Provisioning", miteConnected: false }), NOW);
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;
    const jenkins = model.nodes[2];
    expect(jenkins.lamp).toBe("warn");
    expect(jenkins.steps).toEqual([
      { label: "Provisioning", state: "now" },
      { label: "Running", state: "todo" },
      { label: "Connected", state: "todo" },
    ]);
    expect(model.rails.applyToJenkins).toBe("busy");
  });

  it("waking Running: Provisioning done, Running now, Connected todo", () => {
    const model = buildDeliveryNodes(baseCtrl({ phase: "Running", miteConnected: false }), NOW);
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;
    const jenkins = model.nodes[2];
    expect(jenkins.steps).toEqual([
      { label: "Provisioning", state: "done" },
      { label: "Running", state: "now" },
      { label: "Connected", state: "todo" },
    ]);
  });

  it("waking Pending: warn lamp, all steps not-done with Provisioning upcoming, and NOT 'mite disconnected'", () => {
    const model = buildDeliveryNodes(baseCtrl({ phase: "Pending", miteConnected: false }), NOW);
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;
    const jenkins = model.nodes[2];
    // Pending is an intentional wake phase — amber lamp, never the
    // unexpected-offline red lamp.
    expect(jenkins.lamp).toBe("warn");
    expect(jenkins.line).toBe("waking…");
    expect(jenkins.line).not.toBe("mite disconnected");
    // Nothing has started yet: all three steps are not-done, Provisioning is
    // the upcoming/current step.
    expect(jenkins.steps).toEqual([
      { label: "Provisioning", state: "now" },
      { label: "Running", state: "todo" },
      { label: "Connected", state: "todo" },
    ]);
    expect(jenkins.steps?.every((s) => s.state !== "done")).toBe(true);
    expect(model.rails.applyToJenkins).toBe("busy");
  });

  it("rail priority: failed apply (upstream bad) breaks the rail to a waking jenkins (downstream warn) as 'off', not 'busy'", () => {
    const model = buildDeliveryNodes(
      baseCtrl({
        phase: "Provisioning",
        miteConnected: false,
        lastApplyResult: {
          hash: "1ada258a",
          timestamp: "2026-01-01T00:00:00Z",
          succeeded: false,
          sections: [{ name: "config", ok: true }, { name: "rbac", ok: false, error: "denied" }],
        },
      }),
      NOW,
    );
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;
    const [, apply, jenkins] = model.nodes;
    expect(apply.lamp).toBe("bad");
    expect(jenkins.lamp).toBe("warn");
    // "off" wins over "busy" for EITHER end: a failed apply must break the
    // path to a waking jenkins as "off", not render a busy rail.
    expect(model.rails).toEqual({ bundleToApply: "off", applyToJenkins: "off" });
  });

  it("unexpected-offline: bad lamp, mite disconnected line", () => {
    const model = buildDeliveryNodes(baseCtrl({ miteConnected: false, lastSeen: "2026-07-17T14:20:00Z" }), NOW);
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;
    const jenkins = model.nodes[2];
    expect(jenkins.lamp).toBe("bad");
    expect(jenkins.line).toBe("mite disconnected");
    expect(jenkins.sub?.[0].t).toBe("last heartbeat ");
    expect(jenkins.sub?.[1].t).toBe("5m ago");
    expect(model.rails.applyToJenkins).toBe("off");
  });

  it("bundle line falls back through effectiveBundle → composedBundleRef → starter", () => {
    const viaRef = buildDeliveryNodes(baseCtrl({ effectiveBundle: undefined }), NOW);
    expect(viaRef.kind).toBe("pipeline");
    if (viaRef.kind === "pipeline") expect(viaRef.nodes[0].line).toBe("getting-started-bundle");

    const starter = buildDeliveryNodes(baseCtrl({ effectiveBundle: undefined, composedBundleRef: undefined }), NOW);
    expect(starter.kind).toBe("pipeline");
    if (starter.kind === "pipeline") expect(starter.nodes[0].line).toBe("varroa-starter");
  });

  it("apply sub-line renders 'manual' / 'idle' modes instead of 'automatic · every …'", () => {
    const manual = buildDeliveryNodes(
      baseCtrl({ reconciliationPolicy: { mode: "manual", interval: "30s" } }),
      NOW,
    );
    if (manual.kind !== "pipeline") return;
    expect(manual.nodes[1].sub?.[0].t).toBe("manual · ");

    const idle = buildDeliveryNodes(
      baseCtrl({ reconciliationPolicy: { mode: "idle", interval: "30s" } }),
      NOW,
    );
    if (idle.kind !== "pipeline") return;
    expect(idle.nodes[1].sub?.[0].t).toBe("idle · ");

    // automatic mode (and the default when mode is undefined) keeps the interval.
    const auto = buildDeliveryNodes(
      baseCtrl({ reconciliationPolicy: { mode: "automatic", interval: "5m" } }),
      NOW,
    );
    if (auto.kind !== "pipeline") return;
    expect(auto.nodes[1].sub?.[0].t).toBe("automatic · every 5m · ");

    const defaulted = buildDeliveryNodes(baseCtrl({ reconciliationPolicy: undefined }), NOW);
    if (defaulted.kind !== "pipeline") return;
    expect(defaulted.nodes[1].sub?.[0].t).toBe("automatic · every 30s · ");
  });

  it("manual mode with a hash mismatch is 'awaiting manual approval', not 'applying'", () => {
    const model = buildDeliveryNodes(
      baseCtrl({
        reconciliationPolicy: { mode: "manual", interval: "30s" },
        desiredStateHash: "8f31bc07",
        lastApplyResult: { hash: "1ada258a", timestamp: "2026-01-01T00:00:00Z", succeeded: true, sections: [{ name: "config", ok: true }, { name: "rbac", ok: true }] },
      }),
      NOW,
    );
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;

    const apply = model.nodes[1];
    expect(apply.lamp).toBe("warn");
    expect(apply.line).toBe("awaiting manual approval");
    // A parked (not in-flight) change must not render running checks.
    expect(apply.checks?.every((c) => c.state === "ok")).toBe(true);
  });

  it("manual mode with a hash mismatch still shows the rejection when the apply failed", () => {
    const model = buildDeliveryNodes(
      baseCtrl({
        reconciliationPolicy: { mode: "manual", interval: "30s" },
        desiredStateHash: "8f31bc07",
        lastApplyResult: { hash: "1ada258a", timestamp: "2026-01-01T00:00:00Z", succeeded: false, sections: [{ name: "config", ok: true }, { name: "rbac", ok: false, error: "denied" }] },
      }),
      NOW,
    );
    expect(model.kind).toBe("pipeline");
    if (model.kind !== "pipeline") return;

    const apply = model.nodes[1];
    expect(apply.lamp).toBe("bad");
    expect(apply.line).toBe("rbac rejected");
    expect(apply.error).toBe("denied");
    expect(model.rails.bundleToApply).toBe("off");
  });

  it("apply sub-line has no dangling ' · ' separator when there is no apply hash", () => {
    const model = buildDeliveryNodes(
      baseCtrl({
        desiredStateHash: undefined,
        lastApplyResult: { timestamp: "2026-01-01T00:00:00Z", succeeded: true, sections: [] },
      }),
      NOW,
    );
    if (model.kind !== "pipeline") return;
    expect(model.nodes[1].sub).toEqual([{ t: "automatic · every 30s" }]);
  });

  it("bundle sub-line has no dangling ' · ' separator when there is no applied bundle hash", () => {
    const model = buildDeliveryNodes(baseCtrl({ appliedBundleHash: undefined }), NOW);
    if (model.kind !== "pipeline") return;
    expect(model.nodes[0].lamp).toBe("wait");
    expect(model.nodes[0].sub).toEqual([{ t: "composed" }]);
  });
});

describe("DeliveryPipeline", () => {
  it("renders three named nodes left-to-right with connecting rails", () => {
    render(<DeliveryPipeline ctrl={baseCtrl()} />);
    expect(screen.getByTestId("delivery-pipeline")).toBeInTheDocument();
    expect(screen.getByText("Bundle")).toBeInTheDocument();
    expect(screen.getByText("Apply")).toBeInTheDocument();
    expect(screen.getByText("Jenkins")).toBeInTheDocument();
    expect(screen.getByText("getting-started-bundle")).toBeInTheDocument();
    expect(screen.getByTestId("rail-bundle-apply")).toHaveAttribute("data-rail", "flow");
    expect(screen.getByTestId("rail-apply-jenkins")).toHaveAttribute("data-rail", "flow");
  });

  it("renders the jenkins version string from this component only", () => {
    render(<DeliveryPipeline ctrl={baseCtrl()} />);
    expect(screen.getByText("2.555.3 · healthy")).toBeInTheDocument();
  });

  it("renders the Bundle sub-line without a dangling ' · ' separator when there is no applied bundle hash", () => {
    render(<DeliveryPipeline ctrl={baseCtrl({ appliedBundleHash: undefined })} />);
    const bundleNode = document.querySelector('[data-node="bundle"]') as HTMLElement;
    expect(bundleNode).toBeTruthy();
    expect(within(bundleNode).getByText("composed")).toBeInTheDocument();
    expect(within(bundleNode).queryByText(/composed\s*·/)).toBeNull();
  });

  it("manual mode with a hash mismatch renders 'awaiting manual approval', not 'applying new configuration…'", () => {
    render(
      <DeliveryPipeline
        ctrl={baseCtrl({
          reconciliationPolicy: { mode: "manual", interval: "30s" },
          desiredStateHash: "8f31bc07",
          lastApplyResult: { hash: "1ada258a", timestamp: "2026-01-01T00:00:00Z", succeeded: true, sections: [{ name: "config", ok: true }, { name: "rbac", ok: true }] },
        })}
      />,
    );
    expect(screen.getByText("awaiting manual approval")).toBeInTheDocument();
    expect(screen.queryByText("applying new configuration…")).not.toBeInTheDocument();
  });

  it("shows failed apply chips and a working view-diff affordance", () => {
    const onFetchDiff = vi.fn();
    render(
      <DeliveryPipeline
        ctrl={baseCtrl({
          lastApplyResult: { hash: "1ada258a", timestamp: "2026-01-01T00:00:00Z", succeeded: false, sections: [{ name: "rbac", ok: false, error: "denied" }] },
        })}
        onFetchDiff={onFetchDiff}
      />,
    );
    expect(screen.getByText(/✗ rbac/)).toBeInTheDocument();
    expect(screen.getByText("denied")).toBeInTheDocument();
    screen.getByRole("button", { name: /view diff/i }).click();
    expect(onFetchDiff).toHaveBeenCalled();
  });

  it("renders the quiet card instead of the pipeline when hibernated", () => {
    render(<DeliveryPipeline ctrl={baseCtrl({ hibernated: true, phase: "Hibernated" })} />);
    expect(screen.getByTestId("quiet-card")).toBeInTheDocument();
    expect(screen.getByText("Sleeping — scaled to zero")).toBeInTheDocument();
    expect(screen.queryByTestId("delivery-pipeline")).not.toBeInTheDocument();
    expect(screen.getByText(/config preserved at/)).toBeInTheDocument();
    expect(screen.getByText("1ada258a")).toBeInTheDocument();
  });

  it("renders Pending wake phase as an amber 'waking' state, never 'mite disconnected'", () => {
    render(<DeliveryPipeline ctrl={baseCtrl({ phase: "Pending", miteConnected: false })} />);
    expect(screen.getByText("waking…")).toBeInTheDocument();
    // The unexpected-offline copy must not appear during an intentional wake.
    expect(screen.queryByText("mite disconnected")).not.toBeInTheDocument();
    // Step ticker shows all three steps with Provisioning as the current step.
    const jenkinsNode = document.querySelector('[data-node="jenkins"]') as HTMLElement;
    expect(jenkinsNode).toBeTruthy();
    expect(within(jenkinsNode).getByText("Provisioning")).toBeInTheDocument();
    expect(within(jenkinsNode).getByText("Running")).toBeInTheDocument();
    expect(within(jenkinsNode).getByText("Connected")).toBeInTheDocument();
  });

  it("uses the powered-off copy when stopped", () => {
    render(<DeliveryPipeline ctrl={baseCtrl({ powerState: "Stopped", phase: "Stopped" })} />);
    expect(screen.getByText("Powered off")).toBeInTheDocument();
    expect(screen.getByText("Stopped — not serving. Power on to resume.")).toBeInTheDocument();
  });

  it("quiet card shows '— jobs' (not '0 jobs') when there is no observability data", () => {
    render(
      <DeliveryPipeline
        ctrl={baseCtrl({ hibernated: true, phase: "Hibernated", observability: undefined })}
      />,
    );
    expect(screen.getByTestId("quiet-card")).toBeInTheDocument();
    expect(screen.getByText("— jobs")).toBeInTheDocument();
    expect(screen.queryByText("0 jobs")).not.toBeInTheDocument();
  });

  it("quiet card shows a real '0 jobs' when observability reports a zero total", () => {
    render(
      <DeliveryPipeline
        ctrl={baseCtrl({ hibernated: true, phase: "Hibernated", observability: { summary: { totalJobs: 0 } } })}
      />,
    );
    expect(screen.getByText("0 jobs")).toBeInTheDocument();
  });
});
