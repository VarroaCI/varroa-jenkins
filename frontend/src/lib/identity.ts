import type { MeResponse } from "../types/auth";

/** Starts the subject and username of a local account beside OIDC. The BFF refuses this prefix from an IdP. */
export const LOCAL_SUBJECT_PREFIX = "local:";

/** Returns the username without the local-account prefix. */
export function usernameOf(me: MeResponse | null | undefined): string {
  const raw = me?.preferredUsername || me?.subject || "";
  return raw.startsWith(LOCAL_SUBJECT_PREFIX) ? raw.slice(LOCAL_SUBJECT_PREFIX.length) : raw;
}

/** Returns the display name, then the name, then the username. */
export function displayNameOf(me: MeResponse | null | undefined): string {
  return me?.displayName || me?.name || usernameOf(me);
}

/** Returns at most two uppercase initials of the display name, or the fallback. */
export function initialsOf(me: MeResponse | null | undefined, fallback: string): string {
  const out = displayNameOf(me).split(/\s+/).filter(Boolean)
    .map((w) => w[0]).join("").slice(0, 2).toUpperCase();
  return out || fallback;
}

/** Returns the email, else the full principal when it differs from the display name, else "". */
export function secondaryLineOf(me: MeResponse | null | undefined): string {
  if (!me) return "";
  if (me.email) return me.email;
  const principal = me.preferredUsername || me.subject || "";
  return principal && principal !== displayNameOf(me) ? principal : "";
}
