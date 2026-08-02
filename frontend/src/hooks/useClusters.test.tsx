import { describe, it, expect } from "vitest";
import { coreOf } from "./useClusters";
import type { ClusterEntry } from "../types";

const coreCluster: ClusterEntry = {
  name: "core",
  core: true,
  healthy: true,
  lastHeartbeat: "2025-01-01T00:00:00Z",
  operatorVersion: "1.0.0",
  k8sVersion: "1.28",
  controllerCount: 5,
  connectedCount: 4,
  state: "active",
};

const hiveCluster: ClusterEntry = {
  name: "dev-cluster",
  core: false,
  healthy: true,
  lastHeartbeat: "2025-01-01T00:00:00Z",
  operatorVersion: "1.0.0",
  k8sVersion: "1.27",
  controllerCount: 3,
  connectedCount: 3,
  state: "active",
};

describe("coreOf", () => {
  it("finds the core entry", () => {
    expect(coreOf([coreCluster, hiveCluster])).toBe(coreCluster);
  });

  it("returns undefined when no core", () => {
    expect(coreOf([{ ...hiveCluster, name: "nope" }])).toBeUndefined();
  });

  it("returns undefined for undefined", () => {
    expect(coreOf(undefined)).toBeUndefined();
  });

  it("returns undefined for empty", () => {
    expect(coreOf([])).toBeUndefined();
  });
});
