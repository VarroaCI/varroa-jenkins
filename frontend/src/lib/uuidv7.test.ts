import { describe, expect, it } from "vitest";
import { uuidv7 } from "./uuidv7";

// Mirrors the server's uuidv7Millis (internal/api/handlers_pipelines_write.go):
// the first six bytes, big-endian, are the embedded millisecond timestamp.
function embeddedMillis(id: string): number {
  const hex = id.replace(/-/g, "");
  const bytes = hex.slice(0, 12);
  return Number(`0x${bytes}`);
}

describe("uuidv7", () => {
  it("has the canonical 8-4-4-4-12 hyphenated form", () => {
    const id = uuidv7();
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
  });

  it("carries version 7 in the expected nibble", () => {
    const id = uuidv7();
    const versionNibble = id.split("-")[2][0];
    expect(versionNibble).toBe("7");
  });

  it("carries the RFC 4122 variant in the expected bits", () => {
    const id = uuidv7();
    const variantNibble = parseInt(id.split("-")[3][0], 16);
    // Top two bits of that nibble must be "10".
    expect(variantNibble & 0xc).toBe(0x8);
  });

  it("embeds a timestamp within a second of now", () => {
    const before = Date.now();
    const id = uuidv7();
    const after = Date.now();
    const ts = embeddedMillis(id);
    expect(ts).toBeGreaterThanOrEqual(before - 1000);
    expect(ts).toBeLessThanOrEqual(after + 1000);
  });

  it("mints a different value on every call", () => {
    const a = uuidv7();
    const b = uuidv7();
    expect(a).not.toBe(b);
  });
});
