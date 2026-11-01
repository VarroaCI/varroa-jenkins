import { describe, it, expect, vi, afterEach } from "vitest";
import {
  DRAWER_WIDTH_KEY,
  clampDrawerWidth,
  defaultDrawerWidth,
  dragWidth,
  keyWidth,
  maxDrawerWidth,
  readDrawerWidth,
  writeDrawerWidth,
} from "./drawerWidth";

afterEach(() => {
  vi.restoreAllMocks();
  window.localStorage.clear();
});

describe("drawer width bounds", () => {
  it("clamps to the minimum", () => {
    expect(clampDrawerWidth(100, 1600)).toBe(320);
  });

  it("caps the maximum at 40% of the viewport and 720 px", () => {
    expect(maxDrawerWidth(1024)).toBe(409);
    expect(maxDrawerWidth(1600)).toBe(640);
    expect(maxDrawerWidth(2400)).toBe(720);
    expect(clampDrawerWidth(900, 1024)).toBe(409);
  });

  it("the default mirrors the CSS default", () => {
    expect(defaultDrawerWidth(1024)).toBe(320);
    expect(defaultDrawerWidth(1280)).toBe(384);
    expect(defaultDrawerWidth(1920)).toBe(420);
  });
});

describe("keyWidth", () => {
  it("steps with the arrow keys and clamps", () => {
    expect(keyWidth("ArrowLeft", 336, 1600)).toBe(352);
    expect(keyWidth("ArrowRight", 336, 1600)).toBe(320);
    expect(keyWidth("ArrowRight", 320, 1600)).toBe(320);
    expect(keyWidth("ArrowLeft", 409, 1024)).toBe(409);
  });

  it("Home and End set the limits", () => {
    expect(keyWidth("Home", 500, 1600)).toBe(320);
    expect(keyWidth("End", 500, 1600)).toBe(640);
  });

  it("returns null for other keys", () => {
    expect(keyWidth("Enter", 400, 1600)).toBeNull();
  });
});

describe("dragWidth", () => {
  it("grows when the pointer moves left and clamps", () => {
    expect(dragWidth(320, 500, 440, 1600)).toBe(380);
    expect(dragWidth(320, 500, 0, 1024)).toBe(409);
  });

  it("shrinks when the pointer moves right", () => {
    expect(dragWidth(400, 500, 540, 1600)).toBe(360);
  });
});

describe("stored width", () => {
  it("reads a stored number", () => {
    window.localStorage.setItem(DRAWER_WIDTH_KEY, "480");
    expect(readDrawerWidth()).toBe(480);
  });

  it("returns null for a missing, non-numeric, or non-positive value", () => {
    expect(readDrawerWidth()).toBeNull();
    window.localStorage.setItem(DRAWER_WIDTH_KEY, "abc");
    expect(readDrawerWidth()).toBeNull();
    window.localStorage.setItem(DRAWER_WIDTH_KEY, "0");
    expect(readDrawerWidth()).toBeNull();
  });

  it("returns null when the store throws", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => { throw new Error("blocked"); });
    expect(readDrawerWidth()).toBeNull();
  });

  it("writes a rounded width and ignores a throwing store", () => {
    writeDrawerWidth(335.6);
    expect(window.localStorage.getItem(DRAWER_WIDTH_KEY)).toBe("336");
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => { throw new Error("blocked"); });
    expect(() => writeDrawerWidth(400)).not.toThrow();
  });
});
