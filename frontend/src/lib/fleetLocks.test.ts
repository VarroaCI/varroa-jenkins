import { describe, it, expect } from "vitest";
import { holderOf, holdsLock } from "./fleetLocks";
import { createFleetLock } from "../test/factories";

describe("holdsLock", () => {
  it("names a holder for a held row and for a releasing row", () => {
    expect(holdsLock(createFleetLock({ state: "held" }))).toBe(true);
    expect(holdsLock(createFleetLock({ state: "releasing" }))).toBe(true);
  });

  it("names no holder for a waiting row", () => {
    expect(holdsLock(createFleetLock({ state: "waiting" }))).toBe(false);
  });
});

describe("holderOf", () => {
  it("answers the held row of the same lock name for a waiting row", () => {
    const held = createFleetLock({ name: "payments/smoke-lock", state: "held", runId: "run-1" });
    const waiting = createFleetLock({ name: "payments/smoke-lock", state: "waiting", runId: "run-2" });
    expect(holderOf([held, waiting], waiting)).toBe(held);
  });

  it("answers a releasing row as the holder", () => {
    const releasing = createFleetLock({ name: "payments/smoke-lock", state: "releasing", runId: "run-1" });
    const waiting = createFleetLock({ name: "payments/smoke-lock", state: "waiting", runId: "run-2" });
    expect(holderOf([releasing, waiting], waiting)).toBe(releasing);
  });

  it("answers undefined when no row of that name holds the lock", () => {
    const waiting = createFleetLock({ name: "payments/smoke-lock", state: "waiting", runId: "run-2" });
    const other = createFleetLock({ name: "payments/other-lock", state: "held", runId: "run-3" });
    expect(holderOf([waiting, other], waiting)).toBeUndefined();
  });

  it("answers undefined for a row that is not waiting", () => {
    const held = createFleetLock({ name: "payments/smoke-lock", state: "held", runId: "run-1" });
    expect(holderOf([held], held)).toBeUndefined();
  });
});
