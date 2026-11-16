import { describe, it, expect } from "vitest";
import {
  ATTENTION_ROWS,
  attentionBreakdown,
  attentionCount,
  attentionItems,
  failingDetail,
  failingTitle,
  inputDetail,
  inputTitle,
  type AttentionSources,
} from "./attentionItems";
import { clusterEntry, failingJob, NOW, waitingInput } from "../../test/overviewFactories";
import { createControllerListItem } from "../../test/factories";

function sources(over: Partial<AttentionSources> = {}): AttentionSources {
  return {
    waitingInputs: [],
    failingJobs: [],
    clusters: [],
    controllers: [],
    updateCenterUnservable: 0,
    ...over,
  };
}

describe("attentionItems", () => {
  it("orders inputs, failing jobs, clusters, controllers, and the update center", () => {
    const items = attentionItems(
      sources({
        waitingInputs: [waitingInput()],
        failingJobs: [failingJob()],
        clusters: [clusterEntry({ name: "member-b", healthy: false })],
        controllers: [createControllerListItem({ name: "c1", attention: { kind: "failed" } })],
        updateCenterUnservable: 3,
      }),
    );
    expect(items.map((i) => i.kind)).toEqual(["input", "failing", "cluster", "controller", "updateCenter"]);
  });

  it("orders inputs oldest run first and failing jobs newest failure first", () => {
    const older = waitingInput({}, { runId: "old", createdAt: "2026-11-14T09:00:00Z" });
    const newer = waitingInput({}, { runId: "new", createdAt: "2026-11-14T10:00:00Z" });
    const f1 = failingJob({}, { runId: "f1", createdAt: "2026-11-14T08:00:00Z" });
    const f2 = failingJob({}, { runId: "f2", createdAt: "2026-11-14T11:00:00Z" });
    const items = attentionItems(sources({ waitingInputs: [newer, older], failingJobs: [f1, f2] }));
    expect(items.map((i) => i.key)).toEqual(["input:old", "input:new", "failing:f2", "failing:f1"]);
  });

  it("orders controllers by attention precedence, then by name", () => {
    const items = attentionItems(
      sources({
        controllers: [
          createControllerListItem({ name: "z", attention: { kind: "miteUnreachable" } }),
          createControllerListItem({ name: "b", attention: { kind: "failed" } }),
          createControllerListItem({ name: "a", attention: { kind: "failed" } }),
          createControllerListItem({ name: "healthy" }),
        ],
      }),
    );
    expect(items.map((i) => i.key)).toEqual([
      "controller:core/default/a",
      "controller:core/default/b",
      "controller:core/default/z",
    ]);
  });

  it("skips a healthy, draining, or core cluster", () => {
    const items = attentionItems(
      sources({
        clusters: [
          clusterEntry({ name: "ok", healthy: true }),
          clusterEntry({ name: "draining", healthy: false, state: "draining" }),
          clusterEntry({ name: "core", healthy: false, core: true }),
          clusterEntry({ name: "lost", healthy: false }),
        ],
      }),
    );
    expect(items.map((i) => i.key)).toEqual(["cluster:lost"]);
  });

  it("names each kind in the breakdown with plural forms", () => {
    const one = attentionItems(sources({ waitingInputs: [waitingInput()] }));
    expect(attentionBreakdown(one)).toBe("1 approval");
    const many = attentionItems(
      sources({
        waitingInputs: [waitingInput(), waitingInput({}, { runId: "run-42" })],
        failingJobs: [failingJob(), failingJob({}, { runId: "run-x" })],
        clusters: [clusterEntry({ healthy: false })],
        controllers: [createControllerListItem({ attention: { kind: "failed" } })],
        updateCenterUnservable: 1,
      }),
    );
    expect(attentionBreakdown(many)).toBe(
      "2 approvals · 2 failing jobs · 1 cluster · 1 controller · update center",
    );
  });

  it("adds a plus to the count when a server list is cut", () => {
    const items = attentionItems(sources({ waitingInputs: [waitingInput()] }));
    expect(attentionCount(items, false)).toBe("1");
    expect(attentionCount(items, true)).toBe("1+");
  });

  it("caps the panel at eight rows by constant", () => {
    expect(ATTENTION_ROWS).toBe(8);
  });

  it("builds the failing detail with the stage, the test count, and the last pass", () => {
    const text = failingDetail(failingJob(), NOW);
    expect(text).toMatch(/^#212 failed .+? in Test: 4 tests failed · last passed #209 /);
  });

  it("uses the stage error when no test failed", () => {
    const text = failingDetail(
      failingJob({ testsFailed: 0, stageError: "repository not found", failuresSincePass: 2 }),
      NOW,
    );
    expect(text).toContain("in Test: repository not found");
    expect(text).toContain("· 2 failures since #209 passed");
  });

  it("says no passed run on record", () => {
    const one = failingDetail(failingJob({ lastPassed: undefined, failuresSincePass: 1 }), NOW);
    expect(one).toMatch(/· no passed run on record$/);
    const many = failingDetail(failingJob({ lastPassed: undefined, failuresSincePass: 3 }), NOW);
    expect(many).toMatch(/· 3 failures, no passed run on record$/);
  });

  it("names a nested job once in the input and failing titles", () => {
    const nested = { tenant: "smoke-b", tenantFolder: "smoke-b", jobPath: "smoke-b/platform/nightly-audit" };
    const input = waitingInput({}, { ...nested, buildNumber: 41 });
    expect(inputTitle(input)).toBe("smoke-b / platform/nightly-audit #41 waits for approval");
    const failing = failingJob({}, { ...nested, buildNumber: 41 });
    expect(failingTitle(failing)).toBe("smoke-b / platform/nightly-audit is failing");
  });

  it("builds the input detail with the message and the submitter", () => {
    const text = inputDetail(waitingInput({ input: { id: "p", message: "Promote?", submitter: "alice", parameterCount: 0 } }), NOW);
    expect(text.startsWith('"Promote?" · started ')).toBe(true);
    expect(text).toContain(" by nathan");
    expect(text.endsWith("· only alice may answer")).toBe(true);
  });

  it("names the parameter count and never a time the store does not hold", () => {
    const text = inputDetail(waitingInput({ input: { id: "p", parameterCount: 2 } }), NOW);
    expect(text).toContain("· needs 2 values");
    expect(text).not.toMatch(/asked/);
    const plain = inputDetail(
      waitingInput({ input: { id: "p", parameterCount: 0 }, inputCount: 3 }, { actor: undefined }),
      NOW,
    );
    expect(plain).toContain("· 3 inputs wait");
    expect(plain).not.toContain(" by ");
  });
  it("uses the singular noun for one parameter", () => {
    const text = inputDetail(waitingInput({ input: { id: "p", parameterCount: 1 } }), NOW);
    expect(text).toContain("· needs 1 value");
    expect(text).not.toContain("values");
  });

  it("uses the singular noun for one failed test", () => {
    expect(failingDetail(failingJob({ testsFailed: 1 }), NOW)).toContain(": 1 test failed");
  });
});
