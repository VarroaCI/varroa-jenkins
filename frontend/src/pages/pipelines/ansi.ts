/**
 * A small parser for the ANSI select-graphic-rendition (SGR) sequences a
 * build log emits. It covers the 8 basic colours, the 8 bright colours, the
 * 256-colour form, bold, dim, underline, and reset. It drops every other
 * escape sequence from the rendered text, including a background colour,
 * an unrecognized SGR code, and a non-SGR control sequence such as a cursor
 * move.
 *
 * This module renders no HTML. It only splits one line of console text into
 * styled spans; `RunLogViewer` turns each span into a `<span>` with an
 * inline style, because these are literal terminal colours, not app design
 * tokens.
 */

/** One run of text carrying the SGR state active when it was written. */
export interface AnsiSpan {
  text: string;
  fg?: string;
  bold?: boolean;
  dim?: boolean;
  underline?: boolean;
}

// The standard 16-colour terminal palette: the 8 basic colours, then the 8
// bright colours. The 256-colour form's first 16 entries reuse this table.
const BASIC_FG = [
  "#000000", "#CD3131", "#0DBC79", "#E5E510",
  "#2472C8", "#BC3FBC", "#11A8CD", "#E5E5E5",
];
const BRIGHT_FG = [
  "#666666", "#F14C4C", "#23D18B", "#F5F543",
  "#3B8EEA", "#D670D6", "#29B8DB", "#F5F5F5",
];

/** Resolves one 256-colour index to a CSS colour value. */
function ansi256Color(n: number): string {
  if (n < 8) return BASIC_FG[n];
  if (n < 16) return BRIGHT_FG[n - 8];
  if (n < 232) {
    const i = n - 16;
    const r = Math.floor(i / 36);
    const g = Math.floor((i / 6) % 6);
    const b = i % 6;
    const scale = (v: number) => (v === 0 ? 0 : 55 + v * 40);
    return `rgb(${scale(r)},${scale(g)},${scale(b)})`;
  }
  const grey = 8 + (n - 232) * 10;
  return `rgb(${grey},${grey},${grey})`;
}

interface SgrState {
  fg?: string;
  bold?: boolean;
  dim?: boolean;
  underline?: boolean;
}

const EMPTY_STATE: SgrState = {};

/** Applies one SGR parameter sequence (the digits before the final "m") to a state. */
function applySgr(paramStr: string, state: SgrState): SgrState {
  const params = paramStr.split(";").filter((p) => p.length > 0).map(Number);
  if (params.length === 0) params.push(0);
  let next = state;
  for (let i = 0; i < params.length; i++) {
    const code = params[i];
    if (code === 0) {
      next = {};
    } else if (code === 1) {
      next = { ...next, bold: true };
    } else if (code === 2) {
      next = { ...next, dim: true };
    } else if (code === 4) {
      next = { ...next, underline: true };
    } else if (code >= 30 && code <= 37) {
      next = { ...next, fg: BASIC_FG[code - 30] };
    } else if (code >= 90 && code <= 97) {
      next = { ...next, fg: BRIGHT_FG[code - 90] };
    } else if (code === 38 && params[i + 1] === 5 && params[i + 2] != null) {
      next = { ...next, fg: ansi256Color(params[i + 2]) };
      i += 2;
    }
    // Every other code — a background colour, an unrecognized number, the
    // per-attribute resets (22/24/39/...) — carries no supported form and
    // is dropped.
  }
  return next;
}

// Matches one CSI (Control Sequence Introducer) escape: ESC "[" then any
// number of parameter digits and ";", then one letter that names the
// sequence's kind. "m" is SGR; every other final letter (cursor movement,
// erase, and the rest) is a control sequence this parser drops.
const CSI_RE = /\x1b\[([0-9;]*)([A-Za-z])/g;

/** Splits one console line into styled spans, stripping every ANSI escape. */
export function parseAnsiLine(input: string): AnsiSpan[] {
  const spans: AnsiSpan[] = [];
  let state: SgrState = EMPTY_STATE;
  let lastIndex = 0;
  CSI_RE.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = CSI_RE.exec(input)) !== null) {
    const text = input.slice(lastIndex, m.index);
    if (text) spans.push({ text, ...state });
    lastIndex = CSI_RE.lastIndex;
    const [, paramStr, final] = m;
    if (final === "m") state = applySgr(paramStr, state);
  }
  const rest = input.slice(lastIndex);
  if (rest || spans.length === 0) spans.push({ text: rest, ...state });
  return spans;
}
