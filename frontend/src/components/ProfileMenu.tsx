import { useState, useEffect, useRef } from "react";
import { useNavigate } from "react-router-dom";
import { useTheme } from "../context/ThemeContext";
import { useAuth } from "../context/AuthContext";
import { useToast } from "./Toast";
import styles from "./ProfileMenu.module.css";

const ACCENTS = [
  { id: "honey" as const, color: "#C2611C", label: "Honey" },
  { id: "rust" as const, color: "#A23B1E", label: "Carapace" },
  { id: "pollen" as const, color: "#D69214", label: "Pollen" },
  { id: "propolis" as const, color: "#7A4A1F", label: "Propolis" },
];

export function ProfileMenu() {
  const { theme, accent, setTheme, setAccent } = useTheme();
  const [open, setOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();
  const { toast } = useToast();

  const { user: me, isLoading, logout: authLogout } = useAuth();

  const initials = (me?.displayName || me?.name || "")
    .split(" ")
    .map((n) => n[0])
    .join("")
    .slice(0, 2)
    .toUpperCase() || "?";

  useEffect(() => {
    function onClick(e: MouseEvent) {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    }
    if (open) document.addEventListener("click", onClick);
    return () => document.removeEventListener("click", onClick);
  }, [open]);

  const close = () => setOpen(false);

  const handleLogout = async () => {
    close();
    try {
      await authLogout();
    } catch {
      toast("Logout failed");
    }
  };

  return (
    <div className={styles.profileWrap} ref={menuRef}>
      <button className={styles.avatar} onClick={() => setOpen((o) => !o)}>
        {isLoading ? "…" : initials}
      </button>
      <div className={`${styles.menu} ${open ? styles.open : ""}`}>
        <div className={styles.menuHead}>
          <div className={styles.avatarLg}>{isLoading ? "…" : initials}</div>
          <div>
            <div className={styles.menuName}>
              {isLoading ? "Loading…" : me?.displayName || me?.name || "Sign in"}
            </div>
            <div className={styles.menuEmail}>
              {isLoading ? "" : me?.email || "Not authenticated"}
            </div>
          </div>
        </div>
        <div className={styles.menuSep} />
        <button className={styles.menuItem} onClick={() => { close(); navigate("/profile"); }}>
          <span className={styles.ic}>◉</span> My profile
        </button>
        <button className={styles.menuItem} onClick={() => { close(); navigate("/api-keys"); }}>
          <span className={styles.ic}>⚿</span> API Keys
        </button>
        <div className={styles.menuSep} />
        <div className={styles.themeRow}>
          <span className={styles.themeLabel}>Theme</span>
          <div className={styles.seg}>
            <button
              className={theme === "light" ? styles.on : ""}
              onClick={() => setTheme("light")}
            >
              Light
            </button>
            <button
              className={theme === "dark" ? styles.on : ""}
              onClick={() => setTheme("dark")}
            >
              Dark
            </button>
          </div>
        </div>
        <div className={styles.swatches}>
          {ACCENTS.map((a) => (
            <button
              key={a.id}
              className={`${styles.sw} ${accent === a.id ? styles.swOn : ""}`}
              style={{ background: a.color }}
              title={a.label}
              onClick={() => setAccent(a.id)}
            />
          ))}
        </div>
        <div className={styles.menuSep} />
        <button className={`${styles.menuItem} ${styles.danger}`} onClick={handleLogout}>
          <span className={styles.ic}>⏻</span> Log out &amp; clear session
        </button>
      </div>
    </div>
  );
}
