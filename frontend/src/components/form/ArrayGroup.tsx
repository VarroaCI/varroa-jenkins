import { getUiOptions } from "@rjsf/utils";
import type { ArrayFieldTemplateProps } from "@rjsf/utils";
import styles from "./form.module.css";
import { AddButton } from "./IconButtons";

export default function ArrayGroup(props: ArrayFieldTemplateProps) {
  const { title, items, canAdd, onAddClick, disabled, readonly, registry, uiSchema } = props;
  // RJSF's ArrayField passes only `schema.title || name` as the template title
  // — it never consults ui:title for arrays. Read it here so the curated
  // uiSchema's human labels apply to array sub-fields too (e.g. rbacSpec.groups
  // renders "Group-to-role bindings" instead of "groups").
  const uiOptions = getUiOptions(uiSchema);
  const displayTitle = uiOptions.title ?? title;
  return (
    <div className={styles.arrayGroup}>
      {displayTitle && <div className={styles.arrayTitle}>{displayTitle}</div>}
      {items.map((item, i) => (
        <div key={i} className={styles.arrayItem}>
          {item}
        </div>
      ))}
      {canAdd && (
        <AddButton onClick={onAddClick} disabled={disabled || readonly} registry={registry} />
      )}
    </div>
  );
}
