// DrawerResizeHandle is the left edge of the docked assistant drawer. A
// pointer drag or the arrow, Home, and End keys change the drawer width.
import { useRef, useState, type KeyboardEvent, type PointerEvent as ReactPointerEvent } from "react";
import { DRAWER_MIN_WIDTH, dragWidth, keyWidth } from "./drawerWidth";
import styles from "./DrawerResizeHandle.module.css";

type Props = {
  /** width is the current drawer width, in pixels. */
  width: number;
  /** max is the widest the drawer can be at this viewport. */
  max: number;
  /** onResize receives each width while the viewer changes it. */
  onResize(px: number): void;
  /** onCommit receives the final width when the change ends. */
  onCommit(px: number): void;
};

type Drag = { x: number; w: number; last: number };

/** DrawerResizeHandle renders the separator that resizes the docked drawer. */
export function DrawerResizeHandle({ width, max, onResize, onCommit }: Props) {
  const [dragging, setDragging] = useState(false);
  const drag = useRef<Drag | null>(null);

  const onPointerDown = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    // preventDefault stops the browser from focusing the handle, so focus it here.
    e.currentTarget.focus();
    e.currentTarget.setPointerCapture?.(e.pointerId);
    drag.current = { x: e.clientX, w: width, last: width };
    setDragging(true);
  };

  const onPointerMove = (e: ReactPointerEvent<HTMLDivElement>) => {
    const d = drag.current;
    if (!d) return;
    d.last = dragWidth(d.w, d.x, e.clientX, window.innerWidth);
    onResize(d.last);
  };

  const endDrag = (e: ReactPointerEvent<HTMLDivElement>) => {
    const d = drag.current;
    if (!d) return;
    drag.current = null;
    setDragging(false);
    e.currentTarget.releasePointerCapture?.(e.pointerId);
    onCommit(d.last);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const next = keyWidth(e.key, width, window.innerWidth);
    if (next === null) return;
    e.preventDefault();
    onResize(next);
    onCommit(next);
  };

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize the assistant drawer"
      aria-valuemin={DRAWER_MIN_WIDTH}
      aria-valuemax={max}
      aria-valuenow={width}
      aria-valuetext={`${width} pixels`}
      tabIndex={0}
      className={styles.handle}
      data-dragging={dragging ? "true" : undefined}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={endDrag}
      onPointerCancel={endDrag}
      onKeyDown={onKeyDown}
    />
  );
}
