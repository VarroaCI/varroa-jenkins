import type { ReactNode } from "react";
import { Button } from "./Button";
import styles from "./FilterBar.module.css";

export interface FilterOption {
  value: string;
  label: string;
}

/** FilterSelect is a labelled select. The empty value means every option. */
export function FilterSelect({
  id,
  label,
  value,
  options,
  onChange,
  allLabel = "All",
}: {
  id: string;
  label: string;
  value: string;
  options: FilterOption[];
  onChange: (value: string) => void;
  allLabel?: string;
}) {
  return (
    <div className={styles.field}>
      <label htmlFor={id} className={styles.label}>
        {label}
      </label>
      <select id={id} className={styles.select} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">{allLabel}</option>
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </div>
  );
}

/** SearchBox is a labelled text input that reports every change. */
export function SearchBox({
  id,
  label,
  value,
  onChange,
  placeholder,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  return (
    <div className={styles.field}>
      <label htmlFor={id} className={styles.label}>
        {label}
      </label>
      <input
        id={id}
        type="search"
        className={styles.input}
        value={value}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
      />
    </div>
  );
}

/**
 * FilterBar wraps the filter controls of a table in one labelled group. It
 * ends with a Clear action that resets every filter.
 */
export function FilterBar({
  children,
  onClear,
  clearDisabled,
}: {
  children: ReactNode;
  onClear: () => void;
  clearDisabled: boolean;
}) {
  return (
    <div role="group" aria-label="Filters" className={styles.bar}>
      {children}
      <Button size="sm" variant="ghost" onClick={onClear} disabled={clearDisabled}>
        Clear
      </Button>
    </div>
  );
}
