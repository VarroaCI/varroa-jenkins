import type { ArrayFieldTemplateProps } from "@rjsf/utils";
import styles from "./form.module.css";

export default function ArrayGroup(props: ArrayFieldTemplateProps) {
  const { title, items, canAdd, onAddClick } = props;
  return (
    <div className={styles.arrayGroup}>
      {title && <div className={styles.arrayTitle}>{title}</div>}
      {items.map((item, i) => (
        <div key={i} className={styles.arrayItem}>
          {item}
        </div>
      ))}
      {canAdd && (
        <button className={styles.removeBtn} onClick={onAddClick} type="button" style={{ marginTop: 8 }}>
          + Add
        </button>
      )}
    </div>
  );
}
