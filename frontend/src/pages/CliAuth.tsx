import { useState, useEffect, useCallback } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { useAuth } from "../context/AuthContext";
import { bffFetch } from "../hooks/useApi";
import { Button } from "../components/Button";
import { Card } from "../components/Card";
import LoadingSpinner from "../components/LoadingSpinner";
import styles from "./CliAuth.module.css";

export default function CliAuth() {
  const queryClient = useQueryClient();

  // Parse query parameters (always, at top level).
  const params = new URLSearchParams(location.search);
  const port = Number(params.get("port"));
  const state = params.get("state") ?? "";
  const rawName = (params.get("name") ?? "").replace(/[\x00-\x1f\x7f]/g, "").slice(0, 128);
  const keyName = rawName || "varroactl";
  const valid = Number.isInteger(port) && port >= 1 && port <= 65535 && state !== "";

  const { isAuthenticated, isLoading, authMode, user } = useAuth();

  // Form state for inline login.
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [loginError, setLoginError] = useState("");
  const [inlineSubmitted, setInlineSubmitted] = useState(false);
  const [mintError, setMintError] = useState<string | null>(null);
  const [denied, setDenied] = useState(false);

  const cb = useCallback(
    (q: string) => `http://127.0.0.1:${port}/callback?state=${encodeURIComponent(state)}${q}`,
    [port, state],
  );

  // OIDC redirect: done via useEffect to avoid side-effects in render.
  useEffect(() => {
    if (!valid) return;
    if (isLoading) return;
    if (!isAuthenticated && authMode === "oidc") {
      window.location.href =
        "/login?state=" + encodeURIComponent(location.pathname + location.search);
    }
  }, [valid, isLoading, isAuthenticated, authMode]);

  // Invalid query params — error card, NEVER redirect.
  if (!valid) {
    return (
      <Card title="Invalid CLI Login Request">
        <p className={styles.copy}>
          The request is missing required parameters or uses an invalid port.
        </p>
        <p className={styles.copySmall}>
          Run <code>varroactl login</code> again from your terminal.
        </p>
      </Card>
    );
  }

  // Loading auth state.
  if (isLoading) {
    return <LoadingSpinner />;
  }

  // OIDC redirect (the effect has already fired; render nothing while navigating).
  if (!isAuthenticated && authMode === "oidc") {
    return null;
  }

  // Inline login form for local/ldap auth.
  if (!isAuthenticated && (authMode === "local" || authMode === "ldap")) {
    if (inlineSubmitted) {
      return <LoadingSpinner />;
    }

    const handleLogin = async (e: React.FormEvent) => {
      e.preventDefault();
      setLoginError("");
      setInlineSubmitted(true);
      try {
        const res = await bffFetch<{ id_token: string; expires_in: number }>("/login", {
          method: "POST",
          body: JSON.stringify({ username, password }),
        });
        localStorage.setItem("varroa_id_token", res.id_token);
        localStorage.setItem("varroa_user", username);
        await queryClient.invalidateQueries({ queryKey: ["me"] });
        // Stay on page — do NOT navigate away (would lose the query params).
        // The component will re-render with isAuthenticated=true once /me refetches.
      } catch (err) {
        setLoginError(err instanceof Error ? err.message : "Login failed");
        setInlineSubmitted(false);
      }
    };

    return (
      <Card title="CLI Login">
        <p className={styles.copy}>
          Sign in to authorize the CLI at <code>127.0.0.1:{port}</code>
        </p>
        <form onSubmit={handleLogin} className={styles.form}>
          <input
            type="text"
            placeholder="Username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            className={styles.input}
            autoComplete="username"
          />
          <input
            type="password"
            placeholder="Password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            className={styles.input}
            autoComplete="current-password"
          />
          {loginError && (
            <p className={styles.error}>{loginError}</p>
          )}
          <Button variant="primary" type="submit">
            Sign in
          </Button>
        </form>
      </Card>
    );
  }

  // Authenticated — confirm card.
  if (isAuthenticated) {
    if (denied) {
      return (
        <Card title="Request Denied">
          <p>Request denied — you may close this tab.</p>
        </Card>
      );
    }

    if (mintError) {
      return (
        <Card title="Error">
          <p className={styles.error}>{mintError}</p>
          <p className={styles.copySmall}>
            Run <code>varroactl login</code> again from your terminal.
          </p>
        </Card>
      );
    }

    const userEmail = user?.email ?? "unknown";

    const handleApprove = async () => {
      try {
        const res = await bffFetch<{ token: string }>("/me/apikeys", {
          method: "POST",
          body: JSON.stringify({ name: keyName }),
        });
        window.location.replace(cb(`&token=${encodeURIComponent(res.token)}`));
      } catch (err) {
        setMintError(err instanceof Error ? err.message : "Failed to create API key");
      }
    };

    const handleDeny = () => {
      setDenied(true);
      window.location.replace(cb("&error=denied"));
    };

    return (
      <Card title="Authorize CLI Access">
        <p>
          The CLI at <code>127.0.0.1:{port}</code> requests an API key named{" "}
          <strong>{keyName}</strong> for <strong>{userEmail}</strong>.
        </p>
        <div className={styles.actions}>
          <Button variant="primary" onClick={handleApprove}>
            Approve
          </Button>
          <Button variant="ghost" onClick={handleDeny}>
            Deny
          </Button>
        </div>
      </Card>
    );
  }

  return null;
}
