import { useState, useMemo } from "react";
import { useControllers } from "../hooks/useControllers";
import { useClusters, coreOf } from "../hooks/useClusters";
import styles from "./ControllerPicker.module.css";

interface ControllerPickerProps {
  selected: Set<string>;
  onChange: (selected: Set<string>) => void;
  hasGlobalEvents: boolean;
}

export default function ControllerPicker({
  selected,
  onChange,
  hasGlobalEvents,
}: ControllerPickerProps) {
  const { data: controllers = [] } = useControllers();
  const { data: clusters } = useClusters();
  const core = coreOf(clusters);
  const [search, setSearch] = useState("");

  const candidates = useMemo(() => {
    let list = controllers.map((c) => ({
      key: c.cluster + "/" + c.namespace + "/" + c.name,
      label: c.name,
      subtitle: c.cluster !== core?.name ? `${c.cluster}/${c.namespace}` : c.namespace,
    }));

    if (hasGlobalEvents) {
      list = [
        { key: "__global__", label: "Platform / global", subtitle: "" },
        ...list,
      ];
    }

    if (search.trim()) {
      const q = search.toLowerCase();
      list = list.filter(
        (c) =>
          c.key.toLowerCase().includes(q) ||
          c.label.toLowerCase().includes(q),
      );
    }

    return list;
  }, [controllers, clusters, core, search, hasGlobalEvents]);

  const toggle = (key: string) => {
    const next = new Set(selected);
    if (next.has(key)) {
      next.delete(key);
    } else {
      next.add(key);
    }
    onChange(next);
  };

  const clear = () => onChange(new Set());

  const allSelected = selected.size === 0;

  return (
    <div className={styles.picker}>
      <div className={styles.searchBox}>
        <span>{"⌕"}</span>
        <input
          placeholder="Search controllers..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </div>
      <div className={styles.list}>
        <label className={`${styles.item} ${allSelected ? styles.itemOn : ""}`}>
          <input
            type="checkbox"
            checked={allSelected}
            onChange={clear}
            className={styles.checkbox}
          />
          <span className={styles.itemLabel}>All controllers</span>
        </label>
        {candidates.map((c) => {
          const isChecked = selected.has(c.key);
          return (
            <label
              key={c.key}
              className={`${styles.item} ${isChecked ? styles.itemOn : ""}`}
            >
              <input
                type="checkbox"
                checked={isChecked}
                onChange={() => toggle(c.key)}
                className={styles.checkbox}
              />
              <span className={styles.itemLabel}>{c.label}</span>
              {c.subtitle && (
                <span className={styles.itemSub}>{c.subtitle}</span>
              )}
            </label>
          );
        })}
      </div>
      {selected.size > 0 && (
        <div className={styles.selectedInfo}>
          {selected.size} controller{selected.size !== 1 ? "s" : ""} selected
        </div>
      )}
    </div>
  );
}
