import { describe, it, expect } from "vitest";
import { canCatalogArea, canAdminArea } from "./navPermissions";
import type { Permissions } from "../types/auth";

describe("canCatalogArea", () => {
  it("returns true when user has catalogsources:read", () => {
    const perms: Permissions = {
      global: { catalogsources: { read: true } },
      scopes: [],
    };
    expect(canCatalogArea(perms)).toBe(true);
  });

  it("returns true when user has catalogitems:read", () => {
    const perms: Permissions = {
      global: { catalogitems: { read: true } },
      scopes: [],
    };
    expect(canCatalogArea(perms)).toBe(true);
  });

  it("returns true when user has composedbundles:read", () => {
    const perms: Permissions = {
      global: { composedbundles: { read: true } },
      scopes: [],
    };
    expect(canCatalogArea(perms)).toBe(true);
  });

  it("returns false when no catalog permission is held", () => {
    const perms: Permissions = {
      global: {},
      scopes: [],
    };
    expect(canCatalogArea(perms)).toBe(false);
  });

  it("returns false when perms is undefined", () => {
    expect(canCatalogArea(undefined)).toBe(false);
  });
});

describe("canAdminArea", () => {
  it("returns true when user has global *:*", () => {
    const perms: Permissions = {
      global: { "*": { "*": true } },
      scopes: [],
    };
    expect(canAdminArea(perms)).toBe(true);
  });

  it("returns true when user has global roles:read", () => {
    const perms: Permissions = {
      global: { roles: { read: true } },
      scopes: [],
    };
    expect(canAdminArea(perms)).toBe(true);
  });

  it("returns true when user has global rolebindings:read", () => {
    const perms: Permissions = {
      global: { rolebindings: { read: true } },
      scopes: [],
    };
    expect(canAdminArea(perms)).toBe(true);
  });

  it("returns true when user has global jenkinsroles:read", () => {
    const perms: Permissions = {
      global: { jenkinsroles: { read: true } },
      scopes: [],
    };
    expect(canAdminArea(perms)).toBe(true);
  });

  it("returns true when user has global jenkinsrolebindings:read", () => {
    const perms: Permissions = {
      global: { jenkinsrolebindings: { read: true } },
      scopes: [],
    };
    expect(canAdminArea(perms)).toBe(true);
  });

  it("returns true when user has global provisioningdefaults:update", () => {
    const perms: Permissions = {
      global: { provisioningdefaults: { update: true } },
      scopes: [],
    };
    expect(canAdminArea(perms)).toBe(true);
  });

  it("returns false when no admin permission is held", () => {
    const perms: Permissions = {
      global: {},
      scopes: [],
    };
    expect(canAdminArea(perms)).toBe(false);
  });

  it("returns false when perms is undefined", () => {
    expect(canAdminArea(undefined)).toBe(false);
  });
});
