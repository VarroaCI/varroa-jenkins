import { useCallback } from "react";
import type { WidgetProps } from "@rjsf/utils";
import styles from "./form.module.css";

export default function TextField(props: WidgetProps) {
  const { id, value, onChange, onBlur, placeholder, readonly, required } = props;
  const handleChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => onChange(e.target.value === "" ? undefined : e.target.value),
    [onChange],
  );
  const handleBlur = useCallback(
    (e: React.FocusEvent<HTMLInputElement>) => onBlur && onBlur(id, e.target.value),
    [id, onBlur],
  );
  return (
    <input
      id={id}
      className={styles.input}
      type="text"
      value={value ?? ""}
      onChange={handleChange}
      onBlur={handleBlur}
      placeholder={placeholder}
      readOnly={readonly}
      required={required}
      autoComplete="off"
    />
  );
}
