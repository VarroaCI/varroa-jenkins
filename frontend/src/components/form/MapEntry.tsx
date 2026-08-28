import { useEffect, useRef, useState } from "react";
import {
  ADDITIONAL_PROPERTY_FLAG,
  buttonId,
  TranslatableString,
} from "@rjsf/utils";
import type { WrapIfAdditionalTemplateProps } from "@rjsf/utils";
import { RemoveButton } from "./IconButtons";
import styles from "./form.module.css";
import { useMapKeys } from "./MapKeysContext";
import { useKeyEditReporter } from "./KeyEditContext";

/**
 * The actual map-row body. Extracted from `MapEntry` (which conditionally
 * passes ordinary fields through) so every hook here is called unconditionally
 * for a real map row.
 */
function MapEntryRow(props: WrapIfAdditionalTemplateProps) {
  const {
    id,
    style,
    disabled,
    displayLabel,
    label,
    onKeyRenameBlur,
    onRemoveProperty,
    readonly,
    uiSchema,
    registry,
    children,
    classNames,
  } = props;

  const { translateString } = registry;

  // The sibling key set for this map, provided by ObjectGroup from the map's
  // formData. `null` when no provider is above (defensive: no known siblings,
  // so duplicate rejection is skipped).
  const mapKeys = useMapKeys();
  // Tells the owning card that a key rename is in flight, so the curated tier
  // counts as dirty and a background refetch will not remount the form and
  // drop the typed key. See KeyEditContext.ts.
  const reportKeyEditing = useKeyEditReporter();
  // Whether the current edit session has already been reported (so a session
  // bumps the form draft version exactly once, not once per keystroke).
  const keyEditingReportedRef = useRef(false);

  const keyInputId = `${id}-key`;
  const errorId = `${id}-key-error`;
  const suggestionsId = `${id}-key-suggestions`;
  const keySuggestions = mapKeys?.keySuggestions;
  const hasSuggestions = Boolean(keySuggestions && keySuggestions.length > 0);

  // Controlled key input, seeded from and re-synced to the committed key. A
  // valid rename commits through RJSF, which re-renders this row with the new
  // key as `label`; a rejected rename must snap the input back to `label`.
  const [keyValue, setKeyValue] = useState(label);
  const [rowError, setRowError] = useState<string | null>(null);

  // Re-sync the controlled input when RJSF commits a rename (new `label`).
  useEffect(() => {
    setKeyValue(label);
  }, [label]);

  const handleKeyChange = (event: React.ChangeEvent<HTMLInputElement>) => {
    setKeyValue(event.target.value);
    // First keystroke of a session: tell the card a rename is in flight so the
    // curated tier counts as dirty (a refetch must not remount and drop it).
    if (!keyEditingReportedRef.current) {
      keyEditingReportedRef.current = true;
      reportKeyEditing(true);
    }
  };

  const handleKeyBlur = (event: React.FocusEvent<HTMLInputElement>) => {
    // The edit session ends on blur no matter how it resolves (commit, reject,
    // or no change). The tier may have since become dirty through a committed
    // rename (RJSF onChange), so no version decrement happens here — this only
    // stops reporting, matching how any other form touch leaves the tier dirty
    // until a save re-hydrates it.
    if (keyEditingReportedRef.current) {
      keyEditingReportedRef.current = false;
      reportKeyEditing(false);
    }

    // Keys are normalized once: surrounding whitespace is meaningless in a map
    // key, so the empty and duplicate checks (and the committed value) all use
    // the trimmed form.
    const proposed = event.target.value.trim();

    // Unchanged key on blur: nothing to do, no spurious rename. If the input
    // already snapped back to the committed key after a rejection, the stale
    // row error is cleared here.
    if (proposed === label) {
      setRowError(null);
      return;
    }

    // Empty key: reject, revert, explain.
    if (proposed === "") {
      setKeyValue(label);
      setRowError("Key cannot be empty");
      return;
    }

    // Duplicate key: reject (do NOT call onKeyRenameBlur), revert, explain.
    // The map's key set includes this row's own committed key, so it is
    // excluded before the collision check.
    const siblingKeys = new Set(mapKeys?.keys ?? []);
    siblingKeys.delete(label);
    if (siblingKeys.has(proposed)) {
      setKeyValue(label);
      setRowError(`"${proposed}" is already set`);
      return;
    }

    // Valid rename: commit and clear any row error.
    setRowError(null);
    if (event.target.value !== proposed) {
      // Normalized (trimmed) key — keep the input and the committed value in sync.
      setKeyValue(proposed);
      event.target.value = proposed;
    }
    onKeyRenameBlur(event);
  };

  const keyLabel = translateString(TranslatableString.KeyLabel, [label]);

  return (
    <div className={classNames ? `${styles.mapRow} ${classNames}` : styles.mapRow} style={style}>
      <div className={styles.mapKey}>
        {displayLabel && (
          <label className={styles.label} htmlFor={keyInputId}>
            {keyLabel}
          </label>
        )}
        <input
          id={keyInputId}
          className={styles.input}
          type="text"
          value={keyValue}
          onChange={handleKeyChange}
          onBlur={handleKeyBlur}
          disabled={disabled}
          readOnly={readonly}
          autoComplete="off"
          list={hasSuggestions ? suggestionsId : undefined}
          aria-invalid={rowError ? true : undefined}
          aria-describedby={rowError ? errorId : undefined}
        />
        {/* Suggestions never restrict input — arbitrary keys stay typable. */}
        {hasSuggestions && (
          <datalist id={suggestionsId}>
            {keySuggestions!.map((suggestion) => (
              <option key={suggestion} value={suggestion} />
            ))}
          </datalist>
        )}
        {rowError && (
          <div id={errorId} className={styles.errorText}>
            {rowError}
          </div>
        )}
      </div>

      {/* The value widget renders inside a fieldset so `disabled` makes it (and
          any control it contains) inert — RJSF's stock template only disabled
          the Remove button. `readonly` is handled by the widget itself. */}
      <fieldset className={styles.mapValue} disabled={disabled}>
        {children}
      </fieldset>

      {/* Remove is hidden on a readonly map (design §8: no Add, no Remove, and
          a non-editable key input — it is a view, not a collection). For a
          disabled map it is rendered inert (disabled) like every other control. */}
      {!readonly && (
        <div
          className={styles.mapRemove}
          style={displayLabel ? { marginTop: 20 } : undefined}
        >
          <RemoveButton
            id={buttonId(id, "remove")}
            disabled={disabled}
            onClick={onRemoveProperty}
            uiSchema={uiSchema}
            registry={registry}
          />
        </div>
      )}
    </div>
  );
}

/**
 * `WrapIfAdditionalTemplate` for map rows (RJSF `additionalProperties`).
 *
 * Replaces RJSF's stock template, which leaves the key input editable when
 * `disabled`/`readonly`, and whose rename path silently appends a numeric
 * suffix to a duplicate key (`@rjsf/core` `ObjectField.js` `getAvailableKey`).
 *
 * Here a key rename is committed on blur (`onKeyRenameBlur`) only when the
 * proposed key is non-empty and not already present in a sibling row; otherwise
 * the input reverts and a row-local error is shown. `onKeyRenameBlur` is never
 * called for a rejected key — RJSF has no option that rejects duplicates, so
 * withholding the call is the only way to prevent it.
 *
 * The sibling key set comes from `MapKeysContext`, supplied by `ObjectGroup`
 * from the map's own `formData` — never from module-level state. A row rendered
 * with no provider above it degrades to "no known siblings" (no duplicate
 * rejection) rather than throwing.
 *
 * The value widget is RJSF's job (its errors render through `props.children`);
 * MapEntry adds no value validation.
 *
 * `readonly` renders a pure view: the key input is read-only, the Add control
 * is suppressed (ObjectGroup) and the Remove control is hidden. `disabled`
 * keeps every control visible but inert. See design §8.
 *
 * When `ADDITIONAL_PROPERTY_FLAG` is absent from `schema`, MapEntry is a no-op
 * wrapper: it renders `props.children` unchanged for ordinary fields.
 */
export default function MapEntry(props: WrapIfAdditionalTemplateProps) {
  const { schema, classNames, style, children } = props;

  // Ordinary fields (no ADDITIONAL_PROPERTY_FLAG) pass through untouched.
  if (!(ADDITIONAL_PROPERTY_FLAG in schema)) {
    return (
      <div className={classNames} style={style}>
        {children}
      </div>
    );
  }

  return <MapEntryRow {...props} />;
}
