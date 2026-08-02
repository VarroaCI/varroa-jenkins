import { useLocation, useMatch, useNavigate } from "react-router-dom";
import { useTheme } from "../context/ThemeContext";
import { ProfileMenu } from "./ProfileMenu";
import styles from "./Topbar.module.css";

interface TopbarProps {
  query?: string;
  onQueryChange?: (query: string) => void;
  onSearchOpen?: () => void;
}

export function Topbar({ query = "", onQueryChange = () => {}, onSearchOpen = () => {} }: TopbarProps) {
  const { theme, setTheme } = useTheme();
  const navigate = useNavigate();
  const location = useLocation();
  const controllerMatch = useMatch("/controllers/:cluster/:namespace/:name");
  const crumbs = buildCrumbs(location.pathname, controllerMatch);

  return (
    <header className={styles.topbar}>
      <div className={styles.crumbs}>
        {crumbs.map((c, i) => (
          <span key={i}>
            {i > 0 && <span style={{ opacity: .5 }}> / </span>}
            {c.bold ? <b>{c.label}</b> : c.label}
          </span>
        ))}
      </div>

      <div className={styles.search}>
        <span>⌕</span>
        <input
          aria-label="Global search"
          placeholder="Search controllers, namespaces, groups… (⌘K)"
          value={query}
          onFocus={onSearchOpen}
          onClick={onSearchOpen}
          onChange={(event) => onQueryChange(event.target.value)}
        />
        <span className={styles.kbd}>⌘K</span>
      </div>

      <div className={styles.topActions}>
        <button
          className={styles.iconBtn + " " + styles.dotBadge}
          title="Activity feed"
          onClick={() => navigate("/activity")}
        >
          ◔
        </button>
        <button
          className={styles.iconBtn}
          title="Toggle theme"
          onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
        >
          {theme === "dark" ? "☀" : "☾"}
        </button>
        <ProfileMenu />
      </div>
    </header>
  );
}

interface Crumb {
  label: string;
  bold: boolean;
}

function buildCrumbs(pathname: string, match: ReturnType<typeof useMatch>): Crumb[] {
  if (match) {
    return [
      { label: "Controllers", bold: false },
      { label: `${match.params.cluster ?? ""}/${match.params.namespace ?? ""}/${match.params.name ?? ""}`, bold: true },
    ];
  }

  const segments = pathname.split("/").filter(Boolean);
  if (segments.length === 0) return [{ label: "Dashboard", bold: true }];

  const label = segments[0].charAt(0).toUpperCase() + segments[0].slice(1);
  return [{ label, bold: true }];
}
