import { describe, it, expect } from "vitest";
import { labelFor, missingLabels, STATE_LABELS, HTTP_LABELS } from "./labels";

describe("assistant labels", () => {
  it("labels every registry value", () => {
    expect(missingLabels()).toEqual([]);
  });

  it("falls back to the raw value for an unknown code", () => {
    expect(labelFor("sessionStates", "running")).toBe(STATE_LABELS.running);
    expect(labelFor("httpCodes", "note_too_long")).toBe(HTTP_LABELS.note_too_long);
    expect(labelFor("errorCodes", "something_new")).toBe("something_new");
  });
});
