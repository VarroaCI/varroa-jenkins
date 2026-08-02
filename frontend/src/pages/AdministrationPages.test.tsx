import { describe, expect, it } from "vitest";
import {
  BuiltinRolesPage,
  GroupsPage,
  IdentityPage,
  ProvisioningPage,
  UsersPage,
  VersionsPage,
} from "./AdministrationPages";

describe("administration page wrappers", () => {
  it("creates each dedicated administration page", () => {
    expect(ProvisioningPage()).toBeTruthy();
    expect(VersionsPage()).toBeTruthy();
    expect(IdentityPage()).toBeTruthy();
    expect(UsersPage()).toBeTruthy();
    expect(GroupsPage()).toBeTruthy();
    expect(BuiltinRolesPage()).toBeTruthy();
  });
});
