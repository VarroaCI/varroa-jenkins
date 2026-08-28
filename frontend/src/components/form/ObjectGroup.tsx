import { useCallback, useEffect, useMemo, useRef } from "react";
import { ADDITIONAL_PROPERTY_FLAG, canExpand, getUiOptions } from "@rjsf/utils";
import type { ObjectFieldTemplateProps } from "@rjsf/utils";
import styles from "./form.module.css";
import { AddButton } from "./IconButtons";
import { MapKeysContext } from "./MapKeysContext";

/**
 * `ObjectFieldTemplate` for objects and free-form maps (`additionalProperties`).
 *
 * RJSF hands the template a `properties` array that already contains both fixed
 * schema properties and the currently-rendered additional rows (each tagged with
 * `ADDITIONAL_PROPERTY_FLAG` in the retrieved schema). Fixed properties render
 * directly; the additional rows render inside `MapKeysContext.Provider`, which
 * supplies every `MapEntry` row with the map's current keys (from `formData`)
 * and any `ui:options.keySuggestions` — so a map with no fixed properties still
 * renders its rows plus an Add control.
 */
function isAdditionalRow(schema: ObjectFieldTemplateProps["schema"], name: string): boolean {
  const entry = (schema.properties as Record<string, unknown> | undefined)?.[name];
  return Boolean(
    entry && typeof entry === "object" && ADDITIONAL_PROPERTY_FLAG in entry,
  );
}

export default function ObjectGroup(props: ObjectFieldTemplateProps) {
  const {
    title,
    properties,
    schema,
    uiSchema,
    formData,
    readonly,
    disabled,
    onAddProperty,
    fieldPathId,
    registry,
  } = props;

  // The map's current keys, derived from its own formData. Memoized on the
  // formData reference (which RJSF keeps stable between edits) so a sibling
  // edit doesn't churn every row via a fresh context value.
  const keys = useMemo(
    () => new Set(Object.keys((formData ?? {}) as Record<string, unknown>)),
    [formData],
  );
  const uiOptions = getUiOptions(uiSchema);
  const rawSuggestions = uiOptions.keySuggestions;
  const keySuggestions = Array.isArray(rawSuggestions)
    ? (rawSuggestions as string[])
    : undefined;
  // canExpand consults only additionalProperties/patternProperties,
  // ui:options.expandable and maxProperties — never readonly/disabled — so the
  // template gates on those separately and suppresses the control entirely
  // rather than rendering it disabled.
  const addable = canExpand(schema, uiSchema, formData) && !readonly && !disabled;

  // 3.3: after an add, move focus to the new row's key input. Rather than
  // predicting RJSF's private key naming (`getAvailableKey('newKey', ...)`),
  // the pre-add key set is captured at click time; once `onAddProperty()`
  // commits and re-renders this template with the new row, the current key set
  // is diffed against the capture and the added key is whatever appeared. The
  // new row's key input id is `<mapId>_<addedKey>-key`, built from the map's
  // own `fieldPathId.$id` and the form's id separator. This survives any future
  // change to RJSF's key naming.
  const capturedKeysRef = useRef<Set<string> | null>(null);
  const handleAdd = useCallback(() => {
    capturedKeysRef.current = new Set(
      Object.keys((formData ?? {}) as Record<string, unknown>),
    );
    onAddProperty();
  }, [formData, onAddProperty]);

  // The add commits through RJSF on the next render; a render interleaved
  // between ref-arm and commit (e.g. a flushSync elsewhere in the tree) sees
  // stale formData. Retry for a few renders instead of consuming the ref on
  // the first no-op, but bound the attempts: an armed ref that outlives a
  // genuinely failed add must expire, or a later *rename* of an existing row
  // (which swaps one key for another) would be misread as "the added key".
  const addAttemptsRef = useRef(0);
  useEffect(() => {
    if (!capturedKeysRef.current) return;
    const before = capturedKeysRef.current;
    const after = new Set(
      Object.keys((formData ?? {}) as Record<string, unknown>),
    );
    const addedKey = [...after].find((key) => !before.has(key));
    if (addedKey) {
      capturedKeysRef.current = null;
      addAttemptsRef.current = 0;
      if (fieldPathId?.$id) {
        const separator = registry.globalFormOptions.idSeparator ?? "_";
        const target = document.getElementById(
          `${fieldPathId.$id}${separator}${addedKey}-key`,
        );
        if (target instanceof HTMLInputElement) target.focus();
      }
    } else if (++addAttemptsRef.current >= 3) {
      // The add never landed — expire so a later rename can't steal focus.
      capturedKeysRef.current = null;
      addAttemptsRef.current = 0;
    }
  });

  const fixedProperties = properties.filter((prop) => !isAdditionalRow(schema, prop.name));
  const additionalProperties = properties.filter((prop) => isAdditionalRow(schema, prop.name));

  return (
    <div className={styles.objectGroup}>
      {title && <div className={styles.objectTitle}>{title}</div>}

      {fixedProperties.map((prop) => (
        <div key={prop.content.key || prop.name}>{prop.content}</div>
      ))}

      <MapKeysContext.Provider value={{ keys, keySuggestions }}>
        {additionalProperties.map((prop) => (
          <div key={prop.content.key || prop.name}>{prop.content}</div>
        ))}
        {addable && <AddButton onClick={handleAdd} registry={registry} />}
      </MapKeysContext.Provider>
    </div>
  );
}
