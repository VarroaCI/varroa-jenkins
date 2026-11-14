import { describe, it, expect } from "vitest";
import {
  roleBindingOwner,
  roleBindingOwnerLabel,
  roleBindingOwnerRoute,
  type RoleBindingOwner,
} from "./roleBindingOwner";
import type { ObjectMeta, VarroaRoleBindingSpec } from "../types";

const TA_NAME = "tenantagent-0eb3bb0eec318ce4";
const BS_NAME = "broodschedule-de59698b5963b41984ed191b495eeae7fd55f8d8a1eb7876";

const taLabels = {
  "varroa.dev/tenant-agent": "true",
  "varroa.dev/tenant-agent-namespace": "ns-a",
  "varroa.dev/tenant-agent-name": "agent-a",
};

const bsAnnotations = {
  "varroa.dev/broodschedule-namespace": "ns-a",
  "varroa.dev/broodschedule-name": "nightly",
};

describe("roleBindingOwner", () => {
  it("returns the TenantAgent for its labels and derived name", () => {
    expect(roleBindingOwner({ name: TA_NAME, labels: taLabels })).toEqual({
      kind: "TenantAgent",
      namespace: "ns-a",
      name: "agent-a",
    });
  });

  it("returns the BroodSchedule for its annotations and derived name", () => {
    expect(roleBindingOwner({ name: BS_NAME, annotations: bsAnnotations })).toEqual({
      kind: "BroodSchedule",
      namespace: "ns-a",
      name: "nightly",
    });
  });

  it("returns null for undefined metadata", () => {
    expect(roleBindingOwner(undefined)).toBeNull();
  });

  it("returns null when tenant-agent is not true", () => {
    expect(roleBindingOwner({ name: TA_NAME, labels: { ...taLabels, "varroa.dev/tenant-agent": "false" } })).toBeNull();
  });

  it("returns null for a missing or empty namespace", () => {
    expect(roleBindingOwner({ name: TA_NAME, labels: { ...taLabels, "varroa.dev/tenant-agent-namespace": "" } })).toBeNull();
    const { "varroa.dev/tenant-agent-namespace": _ns, ...noNs } = taLabels;
    expect(roleBindingOwner({ name: TA_NAME, labels: noNs })).toBeNull();
  });

  it("returns null for a renamed copy that keeps the labels", () => {
    expect(roleBindingOwner({ name: "copy-of-agent", labels: taLabels })).toBeNull();
    expect(roleBindingOwner({ name: "copy-of-schedule", annotations: bsAnnotations })).toBeNull();
  });

  it("returns null for an uppercase hash", () => {
    expect(roleBindingOwner({ name: "tenantagent-0EB3BB0EEC318CE4", labels: taLabels })).toBeNull();
  });

  it("returns null for a 15-character TenantAgent hash", () => {
    expect(roleBindingOwner({ name: "tenantagent-0eb3bb0eec318ce", labels: taLabels })).toBeNull();
  });

  it("returns null for a 47-character BroodSchedule hash", () => {
    expect(roleBindingOwner({ name: BS_NAME.slice(0, -1), annotations: bsAnnotations })).toBeNull();
  });

  it("returns null when BroodSchedule keys are labels, not annotations", () => {
    const labels = {
      "varroa.dev/broodschedule-namespace": "ns-a",
      "varroa.dev/broodschedule-name": "nightly",
    };
    expect(roleBindingOwner({ name: BS_NAME, labels } as ObjectMeta)).toBeNull();
  });
});

describe("roleBindingOwnerLabel", () => {
  it("names the kind and the object", () => {
    const ta: RoleBindingOwner = { kind: "TenantAgent", namespace: "ns-a", name: "agent-a" };
    const bs: RoleBindingOwner = { kind: "BroodSchedule", namespace: "ns-a", name: "nightly" };
    expect(roleBindingOwnerLabel(ta)).toBe("Tenant agent: ns-a/agent-a");
    expect(roleBindingOwnerLabel(bs)).toBe("Brood schedule: ns-a/nightly");
  });
});

describe("roleBindingOwnerRoute", () => {
  const bs: RoleBindingOwner = { kind: "BroodSchedule", namespace: "ns-a", name: "nightly" };
  const ta: RoleBindingOwner = { kind: "TenantAgent", namespace: "ns-a", name: "agent-a" };

  it("routes a BroodSchedule to its detail page", () => {
    expect(roleBindingOwnerRoute(bs)).toBe("/brood-schedules/ns-a/nightly");
  });

  it("routes a TenantAgent with one tenant to its agent page", () => {
    const spec: VarroaRoleBindingSpec = {
      subjects: [],
      roleRef: "developer",
      scope: { tenants: [{ name: "tenant-a" }] },
    };
    expect(roleBindingOwnerRoute(ta, spec)).toBe("/pipelines/tenant-a/agent");
  });

  it("returns null for a TenantAgent with no scope", () => {
    expect(roleBindingOwnerRoute(ta, { subjects: [], roleRef: "developer" })).toBeNull();
  });
});
