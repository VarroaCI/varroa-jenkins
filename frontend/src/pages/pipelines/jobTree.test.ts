import { describe, it, expect } from "vitest";
import { buildJobTree, defaultOpenPaths, visibleRows } from "./jobTree";
import { createPipelineSummary, createRunningRun } from "../../test/factories";

describe("buildJobTree", () => {
  it("makes a one-segment path a single job node", () => {
    const tree = buildJobTree([createPipelineSummary({ jobPath: "payments/build" })], [], "payments");
    expect(tree).toHaveLength(1);
    expect(tree[0]).toMatchObject({ name: "build", path: "build", depth: 0, kind: "job" });
  });

  it("produces no node for an item whose path equals the tenant folder", () => {
    const tree = buildJobTree([createPipelineSummary({ jobPath: "payments" })], [], "payments");
    expect(tree).toHaveLength(0);
  });

  it("treats an empty tenant folder as no prefix at all", () => {
    const tree = buildJobTree([createPipelineSummary({ jobPath: "build/nightly" })], [], "");
    expect(tree).toHaveLength(1);
    expect(tree[0]).toMatchObject({ name: "build", kind: "folder" });
    expect(tree[0].children[0]).toMatchObject({ name: "nightly", path: "build/nightly", depth: 1 });
  });

  it("makes a multibranch item own its branch jobs", () => {
    const tree = buildJobTree(
      [
        createPipelineSummary({ jobPath: "payments/checkout-api", kind: "multibranch" }),
        createPipelineSummary({ jobPath: "payments/checkout-api/main" }),
        createPipelineSummary({ jobPath: "payments/checkout-api/dev" }),
      ],
      [],
      "payments",
    );
    expect(tree).toHaveLength(1);
    expect(tree[0]).toMatchObject({ name: "checkout-api", kind: "multibranch", branchCount: 2 });
    expect(tree[0].children.map((c) => c.name)).toEqual(["dev", "main"]);
  });

  it("keeps a multibranch node with no branch, reporting zero branches", () => {
    const tree = buildJobTree(
      [createPipelineSummary({ jobPath: "payments/checkout-api", kind: "multibranch" })],
      [],
      "payments",
    );
    expect(tree[0]).toMatchObject({ kind: "multibranch", branchCount: 0, jobCount: 0 });
    expect(tree[0].children).toHaveLength(0);
  });

  it("renders a declared job that has never run as an ordinary job node", () => {
    const tree = buildJobTree(
      [createPipelineSummary({ jobPath: "payments/release/eu", health: "unknown" })],
      [],
      "payments",
    );
    expect(tree[0].children[0]).toMatchObject({ kind: "job", name: "eu" });
    expect(tree[0].children[0].summary?.health).toBe("unknown");
  });

  it("sorts a folder before a job, then alphabetically inside each group", () => {
    const tree = buildJobTree(
      [
        createPipelineSummary({ jobPath: "payments/zeta" }),
        createPipelineSummary({ jobPath: "payments/api" }),
        createPipelineSummary({ jobPath: "payments/release/eu" }),
      ],
      [],
      "payments",
    );
    expect(tree.map((n) => n.name)).toEqual(["release", "api", "zeta"]);
  });

  it("counts the jobs and the running builds below a node", () => {
    const tree = buildJobTree(
      [
        createPipelineSummary({ jobPath: "payments/release/eu" }),
        createPipelineSummary({ jobPath: "payments/release/us" }),
        createPipelineSummary({ jobPath: "payments/release/apac/tokyo" }),
        createPipelineSummary({ jobPath: "payments/build/nightly" }),
      ],
      [
        createRunningRun({ jobPath: "payments/release/eu" }),
        createRunningRun({ jobPath: "payments/release/apac/tokyo" }),
        createRunningRun({ jobPath: "payments/build/nightly" }),
      ],
      "payments",
    );
    const release = tree.find((n) => n.name === "release");
    expect(release).toMatchObject({ jobCount: 3, runningCount: 2 });
    const build = tree.find((n) => n.name === "build");
    expect(build).toMatchObject({ jobCount: 1, runningCount: 1 });
  });
});

describe("defaultOpenPaths", () => {
  it("returns the foldable paths at depth 0 and depth 1 for two levels", () => {
    const tree = buildJobTree(
      [
        createPipelineSummary({ jobPath: "payments/release/eu/deploy" }),
        createPipelineSummary({ jobPath: "payments/checkout-api", kind: "multibranch" }),
        createPipelineSummary({ jobPath: "payments/checkout-api/main" }),
      ],
      [],
      "payments",
    );
    expect(defaultOpenPaths(tree, 2).sort()).toEqual(["checkout-api", "release", "release/eu"]);
  });

  it("counts a multibranch node as a level", () => {
    const tree = buildJobTree(
      [
        createPipelineSummary({ jobPath: "payments/apps/checkout-api", kind: "multibranch" }),
        createPipelineSummary({ jobPath: "payments/apps/checkout-api/main" }),
      ],
      [],
      "payments",
    );
    expect(defaultOpenPaths(tree, 2).sort()).toEqual(["apps", "apps/checkout-api"]);
  });
});

describe("visibleRows", () => {
  it("leaves a folded node's descendants out of the row list", () => {
    const tree = buildJobTree(
      [
        createPipelineSummary({ jobPath: "payments/release/eu" }),
        createPipelineSummary({ jobPath: "payments/build" }),
      ],
      [],
      "payments",
    );
    expect(visibleRows(tree, new Set()).map((n) => n.path)).toEqual(["release", "build"]);
    expect(visibleRows(tree, new Set(["release"])).map((n) => n.path)).toEqual([
      "release",
      "release/eu",
      "build",
    ]);
  });
});
