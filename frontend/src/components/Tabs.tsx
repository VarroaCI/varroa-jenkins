import { useRef, type KeyboardEvent, type ReactNode } from "react";
import styles from "./Tabs.module.css";

interface Tab {
  id: string;
  label: ReactNode;
}

interface TabsProps {
  tabs: Tab[];
  activeTab: string;
  onSelect: (id: string) => void;
  // Unique per page. Builds the DOM ids that link each tab to its panel.
  idBase: string;
  // Accessible name of the tablist.
  ariaLabel?: string;
}

export function tabDomId(idBase: string, tabId: string) {
  return `${idBase}-tab-${tabId}`;
}

export function panelDomId(idBase: string, tabId: string) {
  return `${idBase}-panel-${tabId}`;
}

// Spread onto the element that holds the active panel.
export function tabPanelProps(idBase: string, activeTab: string) {
  return {
    role: "tabpanel" as const,
    id: panelDomId(idBase, activeTab),
    "aria-labelledby": tabDomId(idBase, activeTab),
    tabIndex: 0,
  };
}

export function Tabs({ tabs, activeTab, onSelect, idBase, ariaLabel }: TabsProps) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const activeIndex = tabs.findIndex((t) => t.id === activeTab);
  const focusIndex = activeIndex >= 0 ? activeIndex : 0;
  const n = tabs.length;

  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return;
    const current = refs.current.findIndex((el) => el === document.activeElement);
    if (current < 0) return;
    let next: number;
    switch (e.key) {
      case "ArrowRight":
        next = (current + 1) % n;
        break;
      case "ArrowLeft":
        next = (current - 1 + n) % n;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = n - 1;
        break;
      default:
        return;
    }
    e.preventDefault();
    refs.current[next]?.focus();
    onSelect(tabs[next].id);
  }

  return (
    <div
      className={styles.tabs}
      role="tablist"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
    >
      {tabs.map((tab, i) => {
        const selected = tab.id === activeTab;
        return (
          <button
            key={tab.id}
            ref={(el) => {
              refs.current[i] = el;
            }}
            type="button"
            role="tab"
            id={tabDomId(idBase, tab.id)}
            aria-selected={selected}
            aria-controls={selected ? panelDomId(idBase, tab.id) : undefined}
            tabIndex={i === focusIndex ? 0 : -1}
            className={`${styles.tab} ${selected ? styles.on : ""}`}
            onClick={() => onSelect(tab.id)}
          >
            {tab.label}
          </button>
        );
      })}
    </div>
  );
}
