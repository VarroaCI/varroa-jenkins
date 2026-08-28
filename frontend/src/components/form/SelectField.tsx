import { useCallback } from "react";
import type { WidgetProps } from "@rjsf/utils";
import styles from "./form.module.css";

export default function SelectField(props: WidgetProps) {
  const { id, value, onChange, onBlur, options, readonly, required } = props;
  const enumOptions = options.enumOptions as { value: string; label: string }[] | undefined;
  const handleChange = useCallback(
    (e: React.ChangeEvent<HTMLSelectElement>) => onChange(e.target.value === "" ? undefined : e.target.value),
    [onChange],
  );
  const handleBlur = useCallback(
    (e: React.FocusEvent<HTMLSelectElement>) => onBlur && onBlur(id, e.target.value),
    [id, onBlur],
  );
  return (
    <select
      id={id}
      className={styles.select}
      value={value ?? ""}
      onChange={handleChange}
      onBlur={handleBlur}
      disabled={readonly}
      required={required}
    >
      {!required && <option value="">-</option>}
      {(enumOptions ?? []).map((opt) => (
        <option key={opt.value} value={opt.value}>
          {opt.label}
        </option>
      ))}
    </select>
  );
}
