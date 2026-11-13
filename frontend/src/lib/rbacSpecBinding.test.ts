import { describe, it, expect } from "vitest";
import { rbacSpecOwner, rbacSpecOwnerLabel } from "./rbacSpecBinding";

const labels = {
  "varroa.dev/managed-by": "varroa-operator",
  "varroa.dev/controller-namespace": "prod-a",
  "varroa.dev/controller-name": "jenkins-1",
};

describe("rbacSpecOwner", () => {
  it("returns the controller of an operator-written binding", () => {
    const owner = rbacSpecOwner({ name: "ctrl-fbff6b43b023e716-admin", labels });
    expect(owner).toEqual({ namespace: "prod-a", name: "jenkins-1" });
    expect(rbacSpecOwnerLabel(owner!)).toBe("prod-a/jenkins-1");
  });

  it("returns null when the metadata is missing", () => {
    expect(rbacSpecOwner(undefined)).toBeNull();
  });

  it.each([
    ["managed-by is missing", { ...labels, "varroa.dev/managed-by": undefined }],
    ["managed-by is local", { ...labels, "varroa.dev/managed-by": "local" }],
    ["the namespace label is missing", { ...labels, "varroa.dev/controller-namespace": undefined }],
    ["the name label is empty", { ...labels, "varroa.dev/controller-name": "" }],
  ])("returns null when %s", (_case, lbls) => {
    expect(rbacSpecOwner({ name: "ctrl-fbff6b43b023e716-admin", labels: lbls as Record<string, string> })).toBeNull();
  });

  it.each([
    ["a renamed copy", "payments-admins"],
    ["an unknown role", "ctrl-fbff6b43b023e716-superuser"],
    ["the dash-joined name", "ctrl-prod-a-jenkins-1-admin"],
    ["an uppercase hash", "ctrl-FBFF6B43B023E716-admin"],
    ["a 15-character hash", "ctrl-fbff6b43b023e71-admin"],
  ])("returns null for %s", (_case, name) => {
    expect(rbacSpecOwner({ name, labels })).toBeNull();
  });
});
