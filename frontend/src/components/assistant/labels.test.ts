import { describe, it, expect } from "vitest";
import { labelFor, missingLabels, stateLine, tokenLine, A1_VALUES, STATE_LABELS, SKIP_LABELS, HTTP_LABELS, STREAM_ERROR_LABELS } from "./labels";

describe("assistant labels", () => {
  it("labels every registry value", () => {
    expect(missingLabels()).toEqual([]);
  });

  it("falls back to the raw value for an unknown code", () => {
    expect(labelFor("sessionStates", "running")).toBe(STATE_LABELS.running);
    expect(labelFor("httpCodes", "note_too_long")).toBe(HTTP_LABELS.note_too_long);
    expect(labelFor("errorCodes", "something_new")).toBe("something_new");
  });

  it("labels every stream error code and falls back to the code", () => {
    for (const code of A1_VALUES.streamErrorCodes) {
      expect(labelFor("streamErrorCodes", code)).toBe(STREAM_ERROR_LABELS[code]);
      expect(STREAM_ERROR_LABELS[code]).not.toBe("");
    }
    expect(A1_VALUES.streamErrorCodes).toHaveLength(7);
    expect(labelFor("streamErrorCodes", "something_new")).toBe("something_new");
  });

  it("builds the footer state line", () => {
    expect(stateLine(null, null)).toBe("No session");
    expect(stateLine("waiting_approval", null)).toBe("State: Waiting for approval");
    expect(stateLine("skipped", "kill_switch")).toBe(`State: ${STATE_LABELS.skipped} · ${SKIP_LABELS.kill_switch}`);
    expect(stateLine("completed", "kill_switch")).toBe("State: Completed");
    expect(stateLine("skipped", null)).toBe("State: Skipped");
  });

  it("builds the footer token line", () => {
    expect(tokenLine(1200, 340, false)).toBe("Tokens: 1200 prompt · 340 completion");
    expect(tokenLine(1200, 340, true)).toBe("Tokens: 1200 prompt · 340 completion (estimated)");
  });
});
