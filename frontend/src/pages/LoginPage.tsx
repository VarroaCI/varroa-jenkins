import { useState, type FormEvent } from "react";
import { useAuth, PROGRESS_COPY } from "../context/AuthContext";
import styles from "./LoginPage.module.css";

interface LoginPageProps {
  /** Optional OIDC error description from provider callback. */
  authError?: string;
}

const PHASES = ["Pending", "Provisioning", "Running", "Connected"] as const;
const CURRENT_PHASE = "Connected";

export function LoginPage({ authError }: LoginPageProps) {
  const { login, authMode } = useAuth();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [invalid, setInvalid] = useState({ username: false, password: false });

  const isOIDC = authMode === "oidc";

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    if (isOIDC) {
      // OIDC mode: navigate to BFF authorization endpoint.
      setSubmitting(true);
      try {
        await login();
      } catch {
        setError("Sign-in failed.");
        setSubmitting(false);
      }
      return;
    }
    setError("");
    setInvalid({ username: false, password: false });
    if (!username.trim() || !password) {
      setError("Username and password are required.");
      setInvalid({ username: !username.trim(), password: !password });
      return;
    }
    setSubmitting(true);
    try {
      await login(username.trim(), password);
    } catch {
      setError("Invalid credentials.");
      setInvalid({ username: true, password: true });
      setSubmitting(false);
    }
  };

  const oidcError = authError
    ? `Authentication failed: ${authError}. Please try signing in again.`
    : "";

  // Prefer the live interactive error over a stale callback error: `authError`
  // comes from a `?error=` query param that fires in any auth mode, so it must
  // not mask a fresh validation/credential error (nor mislead the invalid
  // fields' aria-describedby, which points at #login-alert).
  const message = error || oidcError;

  return (
    <main className={styles.shell}>
      <section className={styles.brandPanel}>
        {/* honeycomb lattice — the one place the palette's wax/comb
            identity gets to be literal rather than implied */}
        <svg className={styles.comb} aria-hidden="true" width="100%" height="100%">
          <defs>
            <pattern id="login-comb" width="72" height="41.5692" patternUnits="userSpaceOnUse">
              <g fill="none" stroke="currentColor" strokeWidth="1">
                <path d="M24,0 L12,20.7846 L-12,20.7846 L-24,0 L-12,-20.7846 L12,-20.7846 Z" />
                <path d="M96,0 L84,20.7846 L60,20.7846 L48,0 L60,-20.7846 L84,-20.7846 Z" />
                <path d="M24,41.5692 L12,62.3538 L-12,62.3538 L-24,41.5692 L-12,20.7846 L12,20.7846 Z" />
                <path d="M96,41.5692 L84,62.3538 L60,62.3538 L48,41.5692 L60,20.7846 L84,20.7846 Z" />
                <path d="M60,20.7846 L48,41.5692 L24,41.5692 L12,20.7846 L24,0 L48,0 Z" />
              </g>
            </pattern>
          </defs>
          <rect width="100%" height="100%" fill="url(#login-comb)" />
        </svg>

        <div className={styles.brandmark}>
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" aria-hidden="true">
            <path d="M12 2.5 L20.5 7.25 L20.5 16.75 L12 21.5 L3.5 16.75 L3.5 7.25 Z" />
            <path d="M12 8 L15.5 10 L15.5 14 L12 16 L8.5 14 L8.5 10 Z" />
          </svg>
          <span className={styles.wordmark}>Varroa</span>
        </div>

        <div className={styles.brandBody}>
          <div className={styles.brandCopy}>
            <h1>Every Jenkins controller in your cluster, under one operator.</h1>
            <p className={styles.lead}>
              Provision, configure, and hibernate controllers declaratively — with a
              mite in every pod reporting home.
            </p>
          </div>

          <div className={styles.phaseRail}>
            <p className={styles.railLabel} id="login-rail-label">
              Controller lifecycle
            </p>
            <ol className={styles.phaseList} aria-labelledby="login-rail-label">
              {PHASES.map((phase) => (
                <li
                  key={phase}
                  className={styles.phase}
                  data-current={phase === CURRENT_PHASE || undefined}
                >
                  <span className={styles.pdot} />
                  {phase}
                </li>
              ))}
            </ol>
          </div>
        </div>
      </section>

      <section className={styles.authPanel}>
        <form className={styles.card} onSubmit={handleSubmit} noValidate>
          <h2 className={styles.title}>Sign in to Varroa</h2>
          <p className={styles.sub}>
            {isOIDC
              ? "Continue with your identity provider."
              : "Sign in with your Varroa account."}
          </p>

          <div className={styles.body}>
            {message && (
              <div className={styles.alert} id="login-alert" role="alert">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true">
                  <circle cx="12" cy="12" r="9" />
                  <path d="M12 7.5v5M12 16.2v.3" />
                </svg>
                <span>{message}</span>
              </div>
            )}

            {!isOIDC ? (
              <>
                <div className={styles.field}>
                  <label htmlFor="username" className={styles.label}>
                    Username
                  </label>
                  <input
                    id="username"
                    type="text"
                    className={`${styles.input} ${styles.focusRing}`}
                    value={username}
                    onChange={(e) => {
                      setUsername(e.target.value);
                      if (invalid.username) setInvalid((v) => ({ ...v, username: false }));
                    }}
                    autoFocus
                    autoComplete="username"
                    disabled={submitting}
                    aria-invalid={invalid.username || undefined}
                    // role="alert" announces once; aria-describedby is what a
                    // screen reader reads back on tabbing to the bad field
                    aria-describedby={invalid.username ? "login-alert" : undefined}
                  />
                </div>

                <div className={styles.field}>
                  <label htmlFor="password" className={styles.label}>
                    Password
                  </label>
                  <input
                    id="password"
                    type="password"
                    className={`${styles.input} ${styles.focusRing}`}
                    value={password}
                    onChange={(e) => {
                      setPassword(e.target.value);
                      if (invalid.password) setInvalid((v) => ({ ...v, password: false }));
                    }}
                    autoComplete="current-password"
                    disabled={submitting}
                    aria-invalid={invalid.password || undefined}
                    aria-describedby={invalid.password ? "login-alert" : undefined}
                  />
                </div>

                <button
                  type="submit"
                  className={`${styles.button} ${styles.focusRing}`}
                  disabled={submitting}
                >
                  {submitting && <span className={styles.spinner} />}
                  {submitting ? "Signing in..." : "Sign in"}
                </button>
              </>
            ) : (
              <>
                <p className={styles.oidcNote}>
                  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" aria-hidden="true">
                    <rect x="4" y="10.5" width="16" height="10" rx="2" />
                    <path d="M8 10.5V7a4 4 0 0 1 8 0v3.5" />
                  </svg>
                  <span>You'll be redirected to your identity provider to sign in, then returned here.</span>
                </p>
                <button
                  type="submit"
                  className={`${styles.button} ${styles.focusRing}`}
                  disabled={submitting}
                >
                  {submitting && <span className={styles.spinner} />}
                  {submitting ? PROGRESS_COPY.redirecting : "Sign in with SSO"}
                </button>
              </>
            )}

            <p className={styles.footNote}>
              {isOIDC ? "varroa-system · single sign-on" : "varroa-system · local accounts"}
            </p>
          </div>
        </form>
      </section>
    </main>
  );
}
