import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { returnFocus } from "./focusReturn";

function el<T extends HTMLElement>(id: string): T {
  return document.getElementById(id) as T;
}

beforeEach(() => {
  document.body.innerHTML = `
    <button id="opener">opener</button>
    <button id="door">door</button>
    <section id="drawer"><button id="close">Close</button></section>
    <a id="page" href="#">page</a>
  `;
});

afterEach(() => {
  document.body.innerHTML = "";
});

describe("returnFocus", () => {
  it("returns focus to the opener when focus is inside the drawer", () => {
    el<HTMLButtonElement>("close").focus();
    returnFocus(el("drawer"), el("opener"), el("door"));
    expect(document.activeElement).toBe(el("opener"));
  });

  it("returns focus to the door when the opener is gone", () => {
    el<HTMLButtonElement>("close").focus();
    el("opener").remove();
    returnFocus(el("drawer"), el("opener"), el("door"));
    expect(document.activeElement).toBe(el("door"));
  });

  it("returns focus to the door when the opener does not take focus", () => {
    el<HTMLButtonElement>("opener").disabled = true;
    el<HTMLButtonElement>("close").focus();
    returnFocus(el("drawer"), el("opener"), el("door"));
    expect(document.activeElement).toBe(el("door"));
  });

  it("returns focus to the door when there is no opener", () => {
    el<HTMLButtonElement>("close").focus();
    returnFocus(el("drawer"), null, el("door"));
    expect(document.activeElement).toBe(el("door"));
  });

  it("returns focus when focus is on no element", () => {
    el<HTMLButtonElement>("close").focus();
    el<HTMLButtonElement>("close").blur();
    expect(document.activeElement).toBe(document.body);
    returnFocus(el("drawer"), el("opener"), el("door"));
    expect(document.activeElement).toBe(el("opener"));
  });

  it("keeps focus outside the drawer where it is", () => {
    el<HTMLAnchorElement>("page").focus();
    returnFocus(el("drawer"), el("opener"), el("door"));
    expect(document.activeElement).toBe(el("page"));
  });

  it("does nothing when neither target can take focus", () => {
    el<HTMLButtonElement>("opener").disabled = true;
    el<HTMLButtonElement>("door").disabled = true;
    el<HTMLButtonElement>("close").focus();
    returnFocus(el("drawer"), el("opener"), el("door"));
    expect(document.activeElement).toBe(el("close"));
  });

  it("skips an opener inside the drawer", () => {
    el<HTMLButtonElement>("close").focus();
    returnFocus(el("drawer"), el("close"), el("door"));
    expect(document.activeElement).toBe(el("door"));
  });
});
