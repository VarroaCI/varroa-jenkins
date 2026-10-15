import { describe, expect, it } from "vitest";
import { parseAnsiLine } from "./ansi";

const ESC = "\x1b";

describe("parseAnsiLine", () => {
  it.each([
    ["reset", `${ESC}[31mred${ESC}[0mplain`, [{ text: "red", fg: "#CD3131" }, { text: "plain" }]],
    ["bold", `${ESC}[1mbold text`, [{ text: "bold text", bold: true }]],
    ["dim", `${ESC}[2mdim text`, [{ text: "dim text", dim: true }]],
    ["underline", `${ESC}[4munderlined`, [{ text: "underlined", underline: true }]],
    ["a basic colour", `${ESC}[32mgreen`, [{ text: "green", fg: "#0DBC79" }]],
    ["a bright colour", `${ESC}[91mbright red`, [{ text: "bright red", fg: "#F14C4C" }]],
    ["the 256-colour form", `${ESC}[38;5;208morange`, [{ text: "orange", fg: "rgb(255,135,0)" }]],
  ] as const)("supported form: %s", (_label, input, expected) => {
    expect(parseAnsiLine(input)).toEqual(expected);
  });

  it("covers all 8 basic colours", () => {
    for (let i = 0; i < 8; i++) {
      const spans = parseAnsiLine(`${ESC}[${30 + i}mx`);
      expect(spans).toEqual([{ text: "x", fg: expect.any(String) }]);
    }
    // Each code maps to a distinct colour.
    const colors = new Set(
      Array.from({ length: 8 }, (_, i) => parseAnsiLine(`${ESC}[${30 + i}mx`)[0].fg),
    );
    expect(colors.size).toBe(8);
  });

  it("covers all 8 bright colours, distinct from the basic set", () => {
    const basic = new Set(
      Array.from({ length: 8 }, (_, i) => parseAnsiLine(`${ESC}[${30 + i}mx`)[0].fg),
    );
    const bright = new Set(
      Array.from({ length: 8 }, (_, i) => parseAnsiLine(`${ESC}[${90 + i}mx`)[0].fg),
    );
    expect(bright.size).toBe(8);
    for (const c of bright) expect(basic.has(c)).toBe(false);
  });

  it("unsupported form: a background colour is dropped", () => {
    expect(parseAnsiLine(`${ESC}[41mred background`)).toEqual([
      { text: "red background" },
    ]);
  });

  it("unsupported form: a non-SGR control sequence is dropped from the text", () => {
    // \x1b[2K is "erase in line" — a cursor/erase control, not an SGR sequence.
    expect(parseAnsiLine(`before${ESC}[2Kafter`)).toEqual([
      { text: "before" },
      { text: "after" },
    ]);
  });

  it("returns one plain span for text with no escape sequence", () => {
    expect(parseAnsiLine("plain console output")).toEqual([
      { text: "plain console output" },
    ]);
  });

  it("returns one empty span for an empty line", () => {
    expect(parseAnsiLine("")).toEqual([{ text: "" }]);
  });

  it("carries active state across multiple sequences", () => {
    const spans = parseAnsiLine(`${ESC}[1m${ESC}[31mbold red${ESC}[0mplain`);
    expect(spans).toEqual([
      { text: "bold red", bold: true, fg: "#CD3131" },
      { text: "plain" },
    ]);
  });
});
