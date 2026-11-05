// focusReturn decides where focus goes when the assistant drawer closes.

/**
 * returnFocus moves focus out of a closing drawer. A returnTo element that the
 * close names takes focus first, wherever focus is. Otherwise it acts only when
 * focus is inside the drawer or on no element, so a close from outside the
 * drawer keeps focus where the user put it. It then tries the opener and then
 * the door, and it stops at the first one that takes focus.
 */
export function returnFocus(
  drawer: HTMLElement | null,
  opener: HTMLElement | null,
  door: HTMLElement | null,
  returnTo: HTMLElement | null = null,
): void {
  if (returnTo !== null && takeFocus(drawer, returnTo)) return;
  const active = document.activeElement;
  const lost = active === null || active === document.body || !active.isConnected;
  const inside = drawer !== null && active !== null && drawer.contains(active);
  if (!lost && !inside) return;
  for (const target of [opener, door]) {
    if (target !== null && takeFocus(drawer, target)) return;
  }
}

/** takeFocus focuses a connected target outside the drawer and reports whether it took focus. */
function takeFocus(drawer: HTMLElement | null, target: HTMLElement): boolean {
  if (!target.isConnected) return false;
  if (drawer !== null && drawer.contains(target)) return false;
  target.focus();
  return document.activeElement === target;
}
