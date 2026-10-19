import { describe, it, expect } from "vitest";
import { approveBlockReason, canDeny } from "./approvalHint";
import type { ApprovalView } from "./transcript";
import type { Permissions } from "../../types/auth";

const now = Date.parse("2026-10-18T10:00:00Z");
const base: ApprovalView = {
  id: "a1", sessionId: "s1", tool: "restart_run", arguments: {}, targetKind: "none", targetRef: "",
  state: "pending", expiresAt: "2026-10-18T11:00:00Z",
};
const perms = {
  global: { "*": { "*": true } },
  tenants: [{ tenant: "payments", grants: [{ verbs: ["write"], jobs: ["*"] }] }],
} as unknown as Permissions;
const noPerms = {} as Permissions;

describe("approveBlockReason", () => {
  it("blocks a resolved, expired, or paused approval", () => {
    expect(approveBlockReason({ ...base, state: "approved" }, "u", "u", perms, false, undefined, now)).toMatch(/resolved/);
    expect(approveBlockReason({ ...base, expiresAt: "2026-10-18T09:00:00Z" }, "u", "u", perms, false, undefined, now)).toMatch(/expired/);
    expect(approveBlockReason(base, "u", "u", perms, true, undefined, now)).toMatch(/paused/);
  });

  it("blocks a viewer who is not the principal", () => {
    expect(approveBlockReason(base, "bob", "alice", perms, false, undefined, now)).toMatch(/Only the person/);
  });

  it("allows an approval with no target check", () => {
    expect(approveBlockReason(base, "u", "u", noPerms, false, undefined, now)).toBeNull();
  });

  it("checks a run target against its tenant", () => {
    const a = { ...base, targetKind: "run", targetRef: "r1" };
    expect(approveBlockReason(a, "u", "u", noPerms, false, undefined, now)).toBe("Loading the run.");
    expect(approveBlockReason(a, "u", "u", noPerms, false, { tenant: "payments", tenantFolder: "", jobPath: "payments/api" }, now))
      .toMatch(/build's pipeline/);
  });

  it("checks a job target from its team/job ref", () => {
    const a = { ...base, targetKind: "job", targetRef: "payments/api" };
    expect(approveBlockReason(a, "u", "u", noPerms, false, undefined, now)).toMatch(/job's pipeline/);
    const bare = { ...base, targetKind: "job", targetRef: "payments" };
    expect(approveBlockReason(bare, "u", "u", noPerms, false, undefined, now)).toMatch(/job's pipeline/);
  });

  it("checks a controller target by its namespace", () => {
    const a = { ...base, targetKind: "controller", targetRef: "core/ci/main" };
    expect(approveBlockReason(a, "u", "u", noPerms, false, undefined, now)).toMatch(/update this controller/);
    expect(approveBlockReason(a, "u", "u", perms, false, undefined, now)).toBeNull();
  });
});

describe("canDeny", () => {
  it("lets only the principal deny a pending, unexpired approval", () => {
    expect(canDeny(base, "u", "u", now)).toBe(true);
    expect(canDeny(base, "bob", "u", now)).toBe(false);
    expect(canDeny({ ...base, state: "denied" }, "u", "u", now)).toBe(false);
    expect(canDeny({ ...base, expiresAt: "2026-10-18T09:00:00Z" }, "u", "u", now)).toBe(false);
  });
});
