import { useEffect, useRef, useState } from "react";
import { Outlet } from "react-router-dom";
import { Sidebar } from "./Sidebar";
import { TabBar } from "./TabBar";
import { Topbar } from "./Topbar";
import { CommandPalette } from "./CommandPalette";
import styles from "./Layout.module.css";

export default function Layout() {
  const [searchOpen, setSearchOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem("varroa-sidebar-collapsed") === "true",
  );

  // Ref avoids stale closure in the keydown effect (dep [] stays clean).
  const searchOpenRef = useRef(searchOpen);
  searchOpenRef.current = searchOpen;

  const handleToggle = () => {
    setCollapsed((prev) => {
      const next = !prev;
      localStorage.setItem("varroa-sidebar-collapsed", String(next));
      return next;
    });
  };

  useEffect(() => {
    const handleKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setSearchOpen((open) => !open);
      } else if (event.key === "Escape") {
        setSearchOpen(false);
      } else if (event.key === "[" && !event.metaKey && !event.ctrlKey && !event.altKey) {
        if (!searchOpenRef.current && event.target instanceof HTMLElement) {
          const el = event.target;
          if (el.tagName !== "INPUT" && el.tagName !== "TEXTAREA" && !el.isContentEditable) {
            event.preventDefault();
            handleToggle();
          }
        }
      }
    };
    document.addEventListener("keydown", handleKey);
    return () => document.removeEventListener("keydown", handleKey);
  }, []);

  return (
    <div className={styles.app}>
      <CommandPalette open={searchOpen} query={searchQuery} onOpenChange={setSearchOpen} onQueryChange={setSearchQuery} />
      <Sidebar collapsed={collapsed} onToggle={handleToggle} />
      <div className={styles.main}>
        <Topbar query={searchQuery} onQueryChange={(query) => { setSearchQuery(query); setSearchOpen(true); }} onSearchOpen={() => setSearchOpen(true)} />
        <div className={styles.content}>
          <Outlet />
        </div>
      </div>
      <TabBar />
    </div>
  );
}
