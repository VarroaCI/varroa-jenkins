// drawerWidth holds the bounds and the stored width of the docked assistant
// drawer. The functions take the viewport width as an argument, so tests do
// not need a browser layout.

/** DOCKED_QUERY is the viewport range where the drawer is docked and resizable. */
export const DOCKED_QUERY = "(min-width: 1024px)";

/** DRAWER_WIDTH_KEY is the localStorage key of the width, per browser. */
export const DRAWER_WIDTH_KEY = "varroa-assistant-drawer-width";

/** DRAWER_MIN_WIDTH is the narrowest drawer, in pixels. */
export const DRAWER_MIN_WIDTH = 320;

/** DRAWER_MAX_WIDTH is the widest drawer, in pixels. */
export const DRAWER_MAX_WIDTH = 720;

/** DRAWER_MAX_SHARE is the largest share of the viewport the drawer can take. */
export const DRAWER_MAX_SHARE = 0.4;

/** DRAWER_STEP is the change of one arrow key press, in pixels. */
export const DRAWER_STEP = 16;

/** maxDrawerWidth is the widest the docked drawer can be at this viewport width. */
export function maxDrawerWidth(viewport: number): number {
  return Math.max(DRAWER_MIN_WIDTH, Math.min(DRAWER_MAX_WIDTH, Math.floor(viewport * DRAWER_MAX_SHARE)));
}

/** clampDrawerWidth keeps a width between the minimum and the maximum. */
export function clampDrawerWidth(px: number, viewport: number): number {
  return Math.round(Math.min(Math.max(px, DRAWER_MIN_WIDTH), maxDrawerWidth(viewport)));
}

/** defaultDrawerWidth is the width when the viewer has not chosen one. It mirrors the CSS default. */
export function defaultDrawerWidth(viewport: number): number {
  return Math.round(Math.min(420, Math.max(320, viewport * 0.3)));
}

/** dragWidth is the width after a pointer drag from startX to clientX. The drawer grows when the pointer moves left. */
export function dragWidth(startWidth: number, startX: number, clientX: number, viewport: number): number {
  return clampDrawerWidth(startWidth + (startX - clientX), viewport);
}

/** keyWidth is the width after a key press, or null when the key does not resize the drawer. */
export function keyWidth(key: string, current: number, viewport: number): number | null {
  switch (key) {
    case "ArrowLeft":
      return clampDrawerWidth(current + DRAWER_STEP, viewport);
    case "ArrowRight":
      return clampDrawerWidth(current - DRAWER_STEP, viewport);
    case "Home":
      return DRAWER_MIN_WIDTH;
    case "End":
      return maxDrawerWidth(viewport);
    default:
      return null;
  }
}

/** readDrawerWidth returns the stored width, or null when none is stored or the store is blocked. */
export function readDrawerWidth(): number | null {
  try {
    const raw = window.localStorage.getItem(DRAWER_WIDTH_KEY);
    if (raw === null) return null;
    const n = Number(raw);
    return Number.isFinite(n) && n > 0 ? n : null;
  } catch {
    return null;
  }
}

/** writeDrawerWidth stores the width. A blocked store is ignored. */
export function writeDrawerWidth(px: number): void {
  try {
    window.localStorage.setItem(DRAWER_WIDTH_KEY, String(Math.round(px)));
  } catch {
    // A blocked store keeps the width for this page only.
  }
}
