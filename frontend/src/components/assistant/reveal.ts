// reveal paces live assistant text. The loop writes a chunk every 500 ms
// (flushEvery in internal/agent/loop/run.go). The view shows each chunk word
// by word, at a rate that empties the backlog in one flush interval.
import { useEffect, useRef, useState } from "react";

/** REVEAL_WINDOW_MS is the time in which the view shows a new backlog. It matches the loop flush interval. */
export const REVEAL_WINDOW_MS = 500;
/** REVEAL_STEP_MS is the shortest time between two visible updates. */
export const REVEAL_STEP_MS = 50;
/** REVEAL_MIN_RATE is the lowest reveal rate, in characters per millisecond. */
export const REVEAL_MIN_RATE = 0.06;
/** REVEAL_SNAP_CHARS is the backlog above which the view shows the text at once. */
export const REVEAL_SNAP_CHARS = 2000;

/** revealRate answers the rate that shows backlog characters in one window. */
export function revealRate(backlog: number): number {
  return Math.max(REVEAL_MIN_RATE, backlog / REVEAL_WINDOW_MS);
}

/** cutAtWord answers the end of the last whole word at or before n. A final text may end inside a word. */
export function cutAtWord(text: string, n: number, final: boolean): number {
  const len = text.length;
  if (n >= len && final) return len;
  let k = Math.max(0, Math.min(Math.floor(n), len));
  while (k > 0 && !isWordEnd(text, k)) k--;
  return k;
}

function isWordEnd(text: string, k: number): boolean {
  return /\s/.test(text[k - 1]) || (k < text.length && /\s/.test(text[k]));
}

/** useRevealedText answers the part of text to show now. It shows all of it when animate is false. */
export function useRevealedText(text: string, animate: boolean, final: boolean): string {
  const shownRef = useRef(animate ? 0 : text.length);
  const [cut, setCut] = useState(() => (animate ? 0 : text.length));
  useEffect(() => {
    const len = text.length;
    if (!animate) { shownRef.current = len; setCut(len); return; }
    shownRef.current = Math.min(shownRef.current, len);
    const backlog = len - shownRef.current;
    if (backlog > REVEAL_SNAP_CHARS) { shownRef.current = len; setCut(len); return; }
    if (backlog <= 0) { const end = cutAtWord(text, len, final); setCut((c) => Math.max(c, end)); return; }
    const rate = revealRate(backlog);
    let handle = 0;
    let prev: number | null = null;
    let lastCommit = -Infinity;
    const tick = (now: number) => {
      if (prev !== null) shownRef.current = Math.min(len, shownRef.current + rate * (now - prev));
      prev = now;
      const reached = shownRef.current >= len;
      if (reached || now - lastCommit >= REVEAL_STEP_MS) {
        lastCommit = now;
        const next = cutAtWord(text, shownRef.current, final);
        setCut((c) => (next > c ? next : c));
      }
      if (!reached) handle = requestAnimationFrame(tick);
    };
    handle = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(handle);
  }, [text, animate, final]);
  if (!animate) return text;
  return text.slice(0, Math.min(cut, text.length));
}
