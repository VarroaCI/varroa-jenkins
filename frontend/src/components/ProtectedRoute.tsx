import { Outlet } from "react-router-dom";
import { useAuth, PROGRESS_COPY } from "../context/AuthContext";
import { LoginPage } from "../pages/LoginPage";
import styles from "./RecoveryState.module.css";

/** Shared auth progress component for waiting/redirect/callback states. */
function AuthProgress({ phase }: { phase: string }) {
  const copy = PROGRESS_COPY[phase] || "Authenticating...";
  return (
    <div className={styles.wrapper}>
      <div className={styles.card}>
        <div className={styles.spinner} aria-hidden="true" />
        <h1 className={styles.title}>{copy}</h1>
        <p className={styles.message}>Please wait while we complete this step.</p>
      </div>
    </div>
  );
}

export function ProtectedRoute() {
  const { phase, authMode, authError } = useAuth();

  // Local/LDAP modes: show login form on checkingSession or loggedOut.
  if ((authMode === "local" || authMode === "ldap") && (phase === "checkingSession" || phase === "loggedOut")) {
    return <LoginPage />;
  }

  // Auth phases that show progress instead of content.
  if (phase === "loadingConfig" ||
      phase === "checkingSession" ||
      phase === "redirecting" ||
      phase === "callback") {
    return <AuthProgress phase={phase} />;
  }

  // Error phase: show LoginPage with safe error message.
  if (phase === "error") {
    return <LoginPage authError={authError} />;
  }

  // Logged out (OIDC): always show LoginPage, never auto-start OIDC.
  if (phase === "loggedOut") {
    return <LoginPage />;
  }

  // Authenticated: render the protected route.
  if (phase === "authenticated") {
    return <Outlet />;
  }

  // Fallback: show nothing while transitioning.
  return <AuthProgress phase="loadingConfig" />;
}
