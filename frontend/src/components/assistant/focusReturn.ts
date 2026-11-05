// focusReturn decides where focus goes when the assistant drawer closes.

/**
 * returnFocus moves focus out of a closing drawer. It acts only when focus is
 * inside the drawer or on no element, so a close from outside the drawer keeps
 * focus where the user put it. It tries the opener and then the door, and it
 * stops at the first one that takes focus.
 */
export function returnFocus(drawer: HTMLElement | null, opener: HTMLElement | null, door: HTMLElement | null): void {
  const active = document.activeElement;
  const lost = active === null || active === document.body || !active.isConnected;
  const inside = drawer !== null && active !== null && drawer.contains(active);
  if (!lost && !inside) return;
  for (const target of [opener, door]) {
    if (target === null || !target.isConnected) continue;
    if (drawer !== null && drawer.contains(target)) continue;
    target.focus();
    if (document.activeElement === target) return;
  }
}
