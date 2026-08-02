import { useCallback } from "react";
import type { WidgetProps } from "@rjsf/utils";
import styles from "./form.module.css";

export default function CheckboxField(props: WidgetProps) {
  const { id, value, onChange, onBlur, readonly } = props;
  const handleChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => onChange(e.target.checked),
    [onChange],
  );
  const handleBlur = useCallback(
    (e: React.FocusEvent<HTMLInputElement>) => onBlur && onBlur(id, e.target.checked),
    [id, onBlur],
  );
  return (
    <div className={styles.checkboxWrapper}>
      <input
        id={id}
        className={styles.checkbox}
        type="checkbox"
        checked={!!value}
        onChange={handleChange}
        onBlur={handleBlur}
        disabled={readonly}
      />
    </div>
  );
}
