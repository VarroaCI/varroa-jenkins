import { afterEach, beforeEach } from "vitest";

// The originals are captured once, at import, so repeated calls restore the
// real jsdom properties.
const originalOffset = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "offsetHeight");
const originalRect = Element.prototype.getBoundingClientRect;

function applyStub(viewportPx: number, rowPx: number): void {
  Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
    configurable: true,
    get(this: HTMLElement) {
      return this.dataset.testid === "controller-table-scroll" ? viewportPx : 0;
    },
  });
  Element.prototype.getBoundingClientRect = function (this: Element): DOMRect {
    const height = this.hasAttribute("data-index") ? rowPx : 0;
    return { x: 0, y: 0, top: 0, left: 0, right: 0, bottom: height, width: 0, height, toJSON() { return this; } } as DOMRect;
  };
}

function restore(): void {
  if (originalOffset) {
    Object.defineProperty(HTMLElement.prototype, "offsetHeight", originalOffset);
  }
  Element.prototype.getBoundingClientRect = originalRect;
}

// stubVirtualViewport gives the picker's scroll container a 720 px viewport
// and every virtual row a 56 px height for each test in the enclosing
// describe block. jsdom performs no layout: the virtualizer renders no rows
// while its viewport measures 0 px, and a row measured 0 px renders every
// row. With the stub, only a bounded window renders.
export function stubVirtualViewport(viewportPx = 720, rowPx = 56): void {
  beforeEach(() => applyStub(viewportPx, rowPx));
  afterEach(restore);
}
