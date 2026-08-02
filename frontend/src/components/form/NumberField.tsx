import { useCallback } from "react";
import type { WidgetProps } from "@rjsf/utils";
import styles from "./form.module.css";

export default function NumberField(props: WidgetProps) {
  const { id, value, onChange, onBlur, placeholder, readonly, required } = props;
  const handleChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const raw = e.target.value;
      onChange(raw === "" ? undefined : Number(raw));
    },
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
      type="number"
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
