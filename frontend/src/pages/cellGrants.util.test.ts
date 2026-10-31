import { describe, expect, it } from "vitest";
import type { CellGrant, CellTenantGrants } from "../types";
import { splitClusterWideGrants } from "./cellGrants.util";

function grant(source: CellGrant["source"], binding: string | undefined): CellGrant {
  return { source, binding, roleRef: "viewer", subjects: [], verbs: ["read"] };
}

function tenant(name: string, grants: CellGrant[], truncated = false): CellTenantGrants {
  return {
    tenant: name,
    itemRole: "item",
    itemPattern: "*",
    truncated,
    grants,
    memberRoles: [],
  };
}

describe("splitClusterWideGrants", () => {
  it("lifts each cluster-wide binding once across tenants", () => {
    const shared = grant("clusterWideBinding", "all-viewers");
    const { clusterWide, tenants } = splitClusterWideGrants([
      tenant("acme", [shared, grant("tenantScopedBinding", "acme-dev")]),
      tenant("beta", [shared]),
    ]);
    expect(clusterWide.map((g) => g.binding)).toEqual(["all-viewers"]);
    expect(tenants[0].own.map((g) => g.binding)).toEqual(["acme-dev"]);
    expect(tenants[1].own).toEqual([]);
  });

  it("keeps tenant-scoped, membership, and federated rows per tenant", () => {
    const rows = [
      grant("tenantScopedBinding", "acme-dev"),
      grant("tenantMembership", undefined),
      grant("federatedBinding", "fed-copy"),
    ];
    const { clusterWide, tenants } = splitClusterWideGrants([tenant("acme", rows)]);
    expect(clusterWide).toEqual([]);
    expect(tenants[0].own).toEqual(rows);
  });

  it("lifts a federated all-tenants copy once across tenants", () => {
    const copy = { ...grant("federatedBinding", "varroa-fed-tenant-all-abc"), allTenants: true };
    const { clusterWide, tenants } = splitClusterWideGrants([
      tenant("acme", [copy, grant("tenantScopedBinding", "acme-dev")]),
      tenant("beta", [copy]),
    ]);
    expect(clusterWide.map((g) => g.binding)).toEqual(["varroa-fed-tenant-all-abc"]);
    expect(tenants[0].own.map((g) => g.binding)).toEqual(["acme-dev"]);
    expect(tenants[1].own).toEqual([]);
  });

  it("lifts an all-tenants copy on a cell with one tenant", () => {
    const copy = { ...grant("federatedBinding", "varroa-fed-tenant-all-abc"), allTenants: true };
    const { clusterWide, tenants } = splitClusterWideGrants([tenant("acme", [copy])]);
    expect(clusterWide.map((g) => g.binding)).toEqual(["varroa-fed-tenant-all-abc"]);
    expect(tenants[0].own).toEqual([]);
  });

  it("keeps a cluster-wide binding and a federated copy of the same name apart", () => {
    const cluster = grant("clusterWideBinding", "shared-name");
    const fed = { ...grant("federatedBinding", "shared-name"), allTenants: true };
    const { clusterWide } = splitClusterWideGrants([tenant("acme", [cluster, fed])]);
    expect(clusterWide.map((g) => g.source)).toEqual(["clusterWideBinding", "federatedBinding"]);
  });

  it("merges the cluster-wide set across tenants when one list is cut", () => {
    const x = grant("clusterWideBinding", "x");
    const y = grant("clusterWideBinding", "y");
    const z = grant("clusterWideBinding", "z");
    const { clusterWide } = splitClusterWideGrants([
      tenant("a", [x, y], true),
      tenant("b", [x, y, z]),
    ]);
    expect(clusterWide.map((g) => g.binding)).toEqual(["x", "y", "z"]);
  });

  it("returns an empty cluster-wide list when no row is cluster-wide", () => {
    const { clusterWide, tenants } = splitClusterWideGrants([
      tenant("acme", [grant("tenantScopedBinding", "acme-dev")]),
    ]);
    expect(clusterWide).toEqual([]);
    expect(tenants).toHaveLength(1);
  });
});
