import { useState } from "react";
import { useQuery, useMutation } from "@tanstack/react-query";
import { useAuth } from "../context/AuthContext";
import { bffFetch } from "../hooks/useApi";
import { useTheme } from "../context/ThemeContext";
import { Card } from "../components/Card";
import { Button } from "../components/Button";
import { KVGrid } from "../components/KVGrid";
import { useToast } from "../components/Toast";
import type { MeResponse } from "../types/auth";
import { changePassword } from "../api/client";
import styles from "./Profile.module.css";

const ACCENTS = [
  { id: "honey" as const, color: "#C2611C", label: "Honey" },
  { id: "rust" as const, color: "#A23B1E", label: "Carapace" },
  { id: "pollen" as const, color: "#D69214", label: "Pollen" },
  { id: "propolis" as const, color: "#7A4A1F", label: "Propolis" },
];

export default function Profile() {
  const { data, isLoading } = useQuery({
    queryKey: ["me"],
    queryFn: () => bffFetch<MeResponse>("/me"),
  });
  const { theme, accent, setTheme, setAccent } = useTheme();
  const { toast } = useToast();
  const [saving, setSaving] = useState(false);
  const auth = useAuth();

  // Password change state (local mode only).
  const [oldPw, setOldPw] = useState("");
  const [newPw, setNewPw] = useState("");
  const [confirmPw, setConfirmPw] = useState("");
  const [pwError, setPwError] = useState("");

  const isLocal = data?.authMode === "local";

  const pwMutation = useMutation({
    mutationFn: () => changePassword({ oldPassword: oldPw, newPassword: newPw }),
    onSuccess: () => {
      toast("Password changed");
      setOldPw(""); setNewPw(""); setConfirmPw(""); setPwError("");
    },
    onError: (e: Error) => setPwError(e.message || "Password change failed"),
  });

  const handleLogout = async () => {
    try {
      await auth.logout();
      toast("Session cleared · redirecting...");
    } catch {
      toast("Logout failed");
    }
  };

  const handlePasswordSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setPwError("");
    if (newPw.length < 8) {
      setPwError("New password must be at least 8 characters");
      return;
    }
    if (newPw !== confirmPw) {
      setPwError("Passwords do not match");
      return;
    }
    pwMutation.mutate();
  };

  const handleThemeChange = async (t: "light" | "dark") => {
    setTheme(t);
    setSaving(true);
    try {
      await bffFetch("/me/preferences", {
        method: "PUT",
        body: JSON.stringify({ theme: t, accent }),
      });
    } catch { /* preferences sync best-effort */ }
    setSaving(false);
  };

  const handleAccentChange = async (a: string) => {
    setAccent(a as typeof accent);
    setSaving(true);
    try {
      await bffFetch("/me/preferences", {
        method: "PUT",
        body: JSON.stringify({ theme, accent: a }),
      });
    } catch { /* best-effort */ }
    setSaving(false);
  };

  if (isLoading) {
    return (
      <div className={styles.page}>
        <div className={styles.loading}>Loading profile...</div>
      </div>
    );
  }

  const me = data;
  const initials = (me?.displayName || me?.name || "U")
    .split(" ")
    .map((n) => n[0])
    .join("")
    .slice(0, 2)
    .toUpperCase();

  return (
    <div className={styles.page}>
      <div className={styles.pageHead}>
        <div>
          <div className={styles.pageTitle}>My profile</div>
          <div className={styles.pageDesc}>Account, preferences, and session</div>
        </div>
      </div>

      <div className={styles.twoCol}>
        <Card title="◉ Account">
          <div className={styles.accountHeader}>
            <div className={styles.avatar}>{initials}</div>
            <div>
              <div className={styles.displayName}>{me?.displayName || me?.name || "—"}</div>
              <div className={styles.muted}>{me?.email}</div>
              <div className={styles.roleTag}>
                <span className={styles.rolePill}>⚿ {me?.groups?.[0] || "user"}</span>
                <span className={styles.muted}> via OIDC</span>
              </div>
            </div>
          </div>
          <KVGrid
            items={[
              { key: "Display name", value: me?.displayName || me?.name || "—" },
              { key: "Username", value: me?.preferredUsername || "—" },
              { key: "OIDC subject", value: me?.subject || "—" },
              { key: "Email", value: me?.email || "—" },
              { key: "Last login", value: me?.lastLogin ? new Date(me.lastLogin).toLocaleString() : "Never" },
              { key: "Default landing", value: "Dashboard" },
            ]}
          />
        </Card>

        <div className={styles.rightCol}>
          <Card title="✎ Appearance">
            <div className={styles.themeRow}>
              <span className={styles.themeLabel}>Theme</span>
              <div className={styles.seg}>
                <button
                  className={theme === "light" ? styles.on : ""}
                  onClick={() => handleThemeChange("light")}
                >
                  Light
                </button>
                <button
                  className={theme === "dark" ? styles.on : ""}
                  onClick={() => handleThemeChange("dark")}
                >
                  Dark
                </button>
              </div>
            </div>
            <div className={styles.accentSection}>
              <div className={styles.accentLabel}>Accent color</div>
              <div className={styles.swatches}>
                {ACCENTS.map((a) => (
                  <button
                    key={a.id}
                    className={`${styles.sw} ${accent === a.id ? styles.swOn : ""}`}
                    style={{ background: a.color }}
                    title={a.label}
                    onClick={() => handleAccentChange(a.id)}
                  />
                ))}
              </div>
            </div>
            {saving && <span className={styles.muted}>Saving...</span>}
          </Card>

          <Card title="⏻ Session">
            <div className={styles.sessionInfo}>
              <div className={styles.statLine}>
                <span className={styles.sk}>Signed in</span>
                <span className={styles.tick}>—</span>
              </div>
              <div className={styles.statLine}>
                <span className={styles.sk}>Session cookie</span>
                <span className={styles.mono}>varroa_token · httpOnly</span>
              </div>
            </div>
            <Button
              variant="ghost"
              onClick={handleLogout}
              className={styles.logoutButton}
            >
              ⏻ Log out &amp; clear session
            </Button>
          </Card>

          {isLocal && (
            <Card title="🔑 Change password">
              <form onSubmit={handlePasswordSubmit}>
                <div className={`${styles.themeRow} ${styles.passwordFields}`}>
                  <input
                    className={styles.formInput}
                    type="password"
                    placeholder="Current password"
                    value={oldPw}
                    onChange={(e) => setOldPw(e.target.value)}
                  />
                  <input
                    className={styles.formInput}
                    type="password"
                    placeholder="New password (min 8 chars)"
                    value={newPw}
                    onChange={(e) => setNewPw(e.target.value)}
                  />
                  <input
                    className={styles.formInput}
                    type="password"
                    placeholder="Confirm new password"
                    value={confirmPw}
                    onChange={(e) => setConfirmPw(e.target.value)}
                  />
                  {pwError && <span className={styles.passwordError}>{pwError}</span>}
                  <Button type="submit" disabled={pwMutation.isPending}>
                    {pwMutation.isPending ? "Changing..." : "Change password"}
                  </Button>
                </div>
              </form>
            </Card>
          )}
        </div>
      </div>
    </div>
  );
}
