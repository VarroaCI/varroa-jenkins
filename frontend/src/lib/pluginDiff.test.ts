import { describe, it, expect } from "vitest";
import { pluginDiff } from "./pluginDiff";

describe("pluginDiff", () => {
  it("detects added and removed plugins", () => {
    const d = pluginDiff(["a@1.0", "b@2.0"], ["b@2.0", "c@3.0"]);
    expect(d.added).toEqual(["c"]);
    expect(d.removed).toEqual(["a"]);
    expect(d.changed).toEqual([]);
  });

  it("detects a version change when both sides carry differing known versions", () => {
    const d = pluginDiff(["role-strategy@742.vb"], ["role-strategy@800.va"]);
    expect(d.added).toEqual([]);
    expect(d.removed).toEqual([]);
    expect(d.changed).toEqual([{ id: "role-strategy", from: "742.vb", to: "800.va" }]);
  });

  it("does not report a change when the version is equal", () => {
    const d = pluginDiff(["a@1.0"], ["a@1.0"]);
    expect(d).toEqual({ added: [], removed: [], changed: [] });
  });

  it("treats an unversioned pin on either side as unchanged, not a change", () => {
    // current versioned, target bare
    expect(pluginDiff(["a@1.0"], ["a"]).changed).toEqual([]);
    // current bare, target versioned
    expect(pluginDiff(["a"], ["a@2.0"]).changed).toEqual([]);
    // both bare
    expect(pluginDiff(["a"], ["a"]).changed).toEqual([]);
    // and none of those are added/removed
    const d = pluginDiff(["a@1.0"], ["a"]);
    expect(d.added).toEqual([]);
    expect(d.removed).toEqual([]);
  });

  it("handles a bare id being added or removed", () => {
    const d = pluginDiff(["a"], ["a", "b"]);
    expect(d.added).toEqual(["b"]);
    expect(d.removed).toEqual([]);
  });

  it("handles empty lists on either side", () => {
    expect(pluginDiff([], ["a@1.0", "b@2.0"])).toEqual({
      added: ["a", "b"],
      removed: [],
      changed: [],
    });
    expect(pluginDiff(["a@1.0"], [])).toEqual({
      added: [],
      removed: ["a"],
      changed: [],
    });
    expect(pluginDiff([], [])).toEqual({ added: [], removed: [], changed: [] });
  });

  it("parses on the last @ so scoped ids survive", () => {
    // no version -> whole thing is the id (only one @, at position 0 style not here);
    // ensure "id@ver" with a single @ still splits correctly
    const d = pluginDiff(["some-plugin@1.2.3"], ["some-plugin@1.2.4"]);
    expect(d.changed).toEqual([{ id: "some-plugin", from: "1.2.3", to: "1.2.4" }]);
  });

  it("sorts each output list by id", () => {
    const d = pluginDiff(["z@1", "a@1"], ["m@1", "b@1"]);
    expect(d.added).toEqual(["b", "m"]);
    expect(d.removed).toEqual(["a", "z"]);
  });
});
