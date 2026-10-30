import { describe, it, expect } from "vitest";
import type { MeResponse } from "../types/auth";
import { usernameOf, displayNameOf, initialsOf, secondaryLineOf } from "./identity";

const local = (over: Partial<MeResponse> = {}): MeResponse => ({
  subject: "local:ui-probe",
  preferredUsername: "local:ui-probe",
  email: "",
  name: "",
  groups: [],
  authMode: "local",
  ...over,
});

describe("usernameOf", () => {
  it("strips the local: prefix", () => {
    expect(usernameOf(local())).toBe("ui-probe");
  });

  it("keeps a bare username", () => {
    expect(usernameOf(local({ subject: "ops", preferredUsername: "ops" }))).toBe("ops");
  });

  it("falls back to the subject when there is no preferredUsername", () => {
    expect(usernameOf(local({ preferredUsername: undefined }))).toBe("ui-probe");
  });

  it("returns an empty string for a missing user", () => {
    expect(usernameOf(null)).toBe("");
    expect(usernameOf(undefined)).toBe("");
  });
});

describe("displayNameOf", () => {
  it("prefers displayName, then name, then username", () => {
    expect(displayNameOf(local({ displayName: "Ops Lead", name: "N" }))).toBe("Ops Lead");
    expect(displayNameOf(local({ name: "Ops Name" }))).toBe("Ops Name");
    expect(displayNameOf(local())).toBe("ui-probe");
  });
});

describe("initialsOf", () => {
  it("gives U for ui-probe", () => {
    expect(initialsOf(local(), "?")).toBe("U");
  });

  it("gives JD for a name with a double space", () => {
    expect(initialsOf(local({ displayName: "John  Doe" }), "?")).toBe("JD");
  });

  it("returns the fallback for an empty identity", () => {
    expect(initialsOf(null, "?")).toBe("?");
    expect(initialsOf(null, "U")).toBe("U");
  });
});

describe("secondaryLineOf", () => {
  it("returns the email when one is set", () => {
    expect(secondaryLineOf(local({ email: "a@b.c" }))).toBe("a@b.c");
  });

  it("returns the principal when it differs from the display name", () => {
    expect(secondaryLineOf(local())).toBe("local:ui-probe");
  });

  it("returns an empty string when the principal equals the display name", () => {
    expect(secondaryLineOf(local({ subject: "ops", preferredUsername: "ops", name: "ops" }))).toBe("");
  });

  it("returns an empty string for a missing user", () => {
    expect(secondaryLineOf(null)).toBe("");
  });
});
