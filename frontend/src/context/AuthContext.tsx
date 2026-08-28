import { createContext, useContext, useCallback, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { bffFetch } from "../hooks/useApi";
import { usePermissions } from "../hooks/usePermissions";
import type { MeResponse, Permissions, AuthConfig } from "../types/auth";

/** Auth phase for the explicit state machine. */
export type AuthPhase =
  | "loadingConfig"
  | "checkingSession"
  | "redirecting"
  | "callback"
  | "authenticated"
  | "loggedOut"
  | "error";

interface AuthState {
  user: MeResponse | null;
  phase: AuthPhase;
  /** True when the /me query is loading (not yet resolved). */
  isLoading: boolean;
  isAuthenticated: boolean;
  permissions: Permissions | undefined;
  authMode?: "oidc" | "local" | "ldap";
  /** OIDC error description from provider callback. */
  authError?: string;
  login: (username?: string, password?: string) => Promise<void>;
  logout: () => Promise<void>;
}

const AuthContext = createContext<AuthState | null>(null);

const PROGRESS_COPY: Record<string, string> = {
  loadingConfig: "Preparing secure sign-in",
  checkingSession: "Checking your session",
  redirecting: "Redirecting to sign in",
  callback: "Signing you in",
};

export { PROGRESS_COPY };

/** Follow a JSON redirect response by navigating to the returned location. */
async function followJSONRedirect(path: string): Promise<void> {
  try {
    const res: { redirect?: string } = await bffFetch(path, { method: "POST" });
    if (res?.redirect) {
      window.location.href = res.redirect;
    } else {
      window.location.href = "/login";
    }
  } catch {
    window.location.href = "/login";
  }
}

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const queryClient = useQueryClient();
  const [phase, setPhase] = useState<AuthPhase>("loadingConfig");
  const [authError, setAuthError] = useState<string | undefined>();
  const prevModeRef = useRef<string | undefined>();

  useEffect(() => {
    const unauthorized = () => {
      try { localStorage.removeItem("varroa_id_token"); localStorage.removeItem("varroa_user"); } catch { /* noop */ }
      queryClient.setQueryData(["me"], null);
      setPhase("loggedOut");
    };
    window.addEventListener("varroa:unauthorized", unauthorized);
    return () => window.removeEventListener("varroa:unauthorized", unauthorized);
  }, [queryClient]);

  // Fetch auth config upfront (unauthenticated, cached).
  const { data: authConfig } = useQuery({
    queryKey: ["auth-config"],
    queryFn: () => bffFetch<AuthConfig>("/auth-config"),
    staleTime: 300_000,
    refetchOnWindowFocus: false,
    retry: 1,
  });

  const mode = authConfig?.mode;
  const prevMode = prevModeRef.current;
  prevModeRef.current = mode;

  // Phase transitions based on mode resolution.
  useEffect(() => {
    if (mode) {
      // Only transition from loadingConfig once the mode is known.
      if (phase === "loadingConfig") {
        setPhase("checkingSession");
      }
    } else if (prevMode !== undefined && mode === undefined) {
      // Mode changed from known to unknown — should not happen in practice.
      setPhase("loadingConfig");
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode]);

  // Check for OIDC callback error in URL.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const err = params.get("error");
    if (err) {
      setAuthError(err);
      setPhase("error");
      // Clean the URL without reload.
      window.history.replaceState({}, "", window.location.pathname);
    }
  }, []);

  const { data: user, isLoading } = useQuery({
    queryKey: ["me"],
    queryFn: () => bffFetch<MeResponse>("/me"),
    retry: 2,
    staleTime: 60_000,
    refetchOnWindowFocus: false,
    // Don't retry on 401 (logged out).
  });

  // Phase transitions based on /me result.
  useEffect(() => {
    if (!isLoading && mode) {
      if (user) {
        setPhase("authenticated");
        setAuthError(undefined);
      } else if (phase === "checkingSession") {
        setPhase("loggedOut");
      }
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isLoading, user, mode]);

  const isAuthenticated = !isLoading && user != null;
  const { data: permissions } = usePermissions(isAuthenticated);

  const login = useCallback(async (username?: string, password?: string) => {
    // Local/LDAP auth: POST to /api/v1/login with credentials.
    if ((mode === "local" || mode === "ldap") && username && password) {
      setPhase("checkingSession");
      const res = await bffFetch<{ id_token: string; expires_in: number }>("/login", {
        method: "POST",
        body: JSON.stringify({ username, password }),
      });
      localStorage.setItem("varroa_id_token", res.id_token);
      localStorage.setItem("varroa_user", username);
      await queryClient.invalidateQueries({ queryKey: ["me"] });
      queryClient.refetchQueries({ queryKey: ["me"] });
      window.location.href = "/";
      return;
    }
    // OIDC: navigate to the BFF authorization endpoint.
    const returnPath = window.location.pathname !== "/login" ? window.location.pathname : "/";
    setPhase("redirecting");
    window.location.href = `/api/v1/auth/login?return=${encodeURIComponent(returnPath)}`;
  }, [mode, queryClient]);

  const logout = useCallback(async () => {
    try {
      localStorage.removeItem("varroa_id_token");
    } catch { /* noop */ }
    try {
      localStorage.removeItem("varroa_user");
    } catch { /* noop */ }
    queryClient.setQueryData(["me"], null);
    setPhase("loggedOut");

    // Call the BFF logout endpoint and follow the JSON redirect.
    await followJSONRedirect("/logout");
  }, [queryClient]);

  return (
    <AuthContext.Provider value={{
      user: user ?? null,
      phase,
      isLoading,
      isAuthenticated,
      permissions,
      authMode: mode,
      authError,
      login,
      logout,
    }}>
      {children}
    </AuthContext.Provider>
  );
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used within an AuthProvider");
  }
  return ctx;
}
