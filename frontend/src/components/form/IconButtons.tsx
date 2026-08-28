import type { IconButtonProps } from "@rjsf/utils";
import styles from "./form.module.css";

/**
 * Shared RJSF `ButtonTemplates` styled with the app's form language.
 *
 * RJSF's stock buttons emit Bootstrap/glyphicon markup (`col-xs` / `btn-add` /
 * `glyphicon-*`) which this app does not ship, so we replace them. The stock
 * icon glyphs are dropped: the Add button carries its own label and the
 * Remove button is a bare × like the rest of the app.
 *
 * Both accept and honour `disabled` (spread through from `ButtonHTMLAttributes`,
 * which `IconButtonProps` extends).
 */
export function AddButton(props: IconButtonProps) {
  const { className, icon, iconType, uiSchema, registry, ...rest } = props;
  return (
    <button type="button" className={styles.addBtn} {...rest}>
      + Add
    </button>
  );
}

export function RemoveButton(props: IconButtonProps) {
  const { className, icon, iconType, uiSchema, registry, ...rest } = props;
  return (
    <button type="button" className={styles.removeBtn} {...rest} aria-label="Remove" title="Remove">
      ×
    </button>
  );
}
