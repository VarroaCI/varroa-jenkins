import { describe, it, expect } from "vitest";
import { federatedCopySource } from "./federatedCopy";

describe("federatedCopySource", () => {
  it("returns null without the federated-from label", () => {
    expect(federatedCopySource("JenkinsRole", undefined)).toBeNull();
    expect(federatedCopySource("JenkinsRole", { name: "r", labels: { other: "x" } })).toBeNull();
    expect(federatedCopySource("JenkinsRoleBinding", { name: "b", labels: { "varroa.dev/federated-from": "" } })).toBeNull();
  });

  it("names the roles of a role copy from the annotation", () => {
    const got = federatedCopySource("JenkinsRole", {
      name: "r",
      labels: { "varroa.dev/federated-from": "alpha" },
      annotations: { "varroa.dev/federated-roles": "alpha,zeta" },
    });
    expect(got).toEqual({ kind: "VarroaRole", names: ["alpha", "zeta"] });
  });

  it("falls back to the label value for a role copy without the annotation", () => {
    const got = federatedCopySource("JenkinsRole", { name: "r", labels: { "varroa.dev/federated-from": "tenant-admin" } });
    expect(got).toEqual({ kind: "VarroaRole", names: ["tenant-admin"] });
  });

  it("names the bindings of a binding copy from the annotation", () => {
    const got = federatedCopySource("JenkinsRoleBinding", {
      name: "b",
      labels: { "varroa.dev/federated-from": "writer" },
      annotations: { "varroa.dev/federated-bindings": "bind-a, bind-b" },
    });
    expect(got).toEqual({ kind: "VarroaRoleBinding", names: ["bind-a", "bind-b"] });
  });

  it("falls back to the VarroaRole label for a binding copy without the annotation", () => {
    const got = federatedCopySource("JenkinsRoleBinding", { name: "b", labels: { "varroa.dev/federated-from": "writer" } });
    expect(got).toEqual({ kind: "VarroaRole", names: ["writer"] });
  });

  it("drops empty names", () => {
    const got = federatedCopySource("JenkinsRole", {
      name: "r",
      labels: { "varroa.dev/federated-from": "alpha" },
      annotations: { "varroa.dev/federated-roles": " alpha , ,zeta," },
    });
    expect(got?.names).toEqual(["alpha", "zeta"]);
  });
});
