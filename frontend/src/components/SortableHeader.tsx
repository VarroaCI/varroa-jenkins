import styles from "./SortableHeader.module.css";

export type SortOrder = "asc" | "desc";

export interface SortableHeaderProps<K extends string> {
  label: string;
  sortKey: K;
  activeKey: K;
  order: SortOrder;
  onSort: (key: K) => void;
  className?: string;
}

/**
 * SortableHeader renders one column header that sorts a table. The native
 * button keeps it keyboard operable. The arrow is decorative, and aria-sort
 * on the header carries the state for assistive technology.
 */
export function SortableHeader<K extends string>({
  label,
  sortKey,
  activeKey,
  order,
  onSort,
  className,
}: SortableHeaderProps<K>) {
  const active = sortKey === activeKey;
  const ariaSort = active ? (order === "asc" ? "ascending" : "descending") : "none";
  const arrow = active ? (order === "asc" ? "▲" : "▼") : "↕";
  return (
    <th scope="col" aria-sort={ariaSort} className={className}>
      <button type="button" className={styles.button} onClick={() => onSort(sortKey)}>
        {label}
        <span aria-hidden="true" className={active ? styles.arrowActive : styles.arrow}>
          {arrow}
        </span>
      </button>
    </th>
  );
}

/**
 * nextSort answers the sort after a header click. A click on the active key
 * flips the order. A click on another key takes that key's first order.
 */
export function nextSort<K extends string>(
  activeKey: K,
  order: SortOrder,
  clicked: K,
  firstOrder: SortOrder,
): { key: K; order: SortOrder } {
  if (clicked === activeKey) {
    return { key: clicked, order: order === "asc" ? "desc" : "asc" };
  }
  return { key: clicked, order: firstOrder };
}
