// useFocusFallback keeps focus in a panel when a control that has focus unmounts.
import { useCallback, type RefCallback } from "react";

/**
 * useFocusFallback returns a ref callback for a control that can unmount while
 * it has focus. When the control unmounts with focus, focus moves to the
 * element in fallbackRef. React runs a ref cleanup before it removes the
 * element from the document, so the cleanup still sees the focus. One callback
 * can serve several controls. The callback must stay stable, because React
 * runs the cleanup of a replaced ref callback on each render. The caller
 * must pass the same ref object on every render, so the callback stays stable.
 */
export function useFocusFallback(fallbackRef: { readonly current: HTMLElement | null }): RefCallback<HTMLElement> {
  return useCallback(
    (el: HTMLElement | null) => {
      if (el === null) return undefined;
      return () => {
        if (document.activeElement === el) fallbackRef.current?.focus();
      };
    },
    [fallbackRef],
  );
}
