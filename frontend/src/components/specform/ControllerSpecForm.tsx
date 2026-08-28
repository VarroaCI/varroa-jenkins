import { useCallback, useEffect, useMemo, useRef } from "react";
import { customizeValidator } from "@rjsf/validator-ajv8";
import type { RJSFSchema, RJSFValidationError } from "@rjsf/utils";
import type Form from "@rjsf/core";
import { useOpenAPISchema, getControllerSpecSchema } from "../../api/openapiSchema";
import { EXCLUDED_FROM_TIER1 } from "./excludedFields";
import { CONTROLLER_SPEC_UI_SCHEMA } from "./controllerUiSchema";
import { VarroaForm } from "./VarroaFormTheme";

const validator = customizeValidator();

interface ControllerSpecFormProps {
  value?: Record<string, unknown>;
  onChange?: (value: Record<string, unknown>) => void;
  /**
   * Live validation errors, lifted upward from the `onChange` event RJSF
   * already fires — `IChangeEvent` carries `errors`/`errorSchema`, so there is
   * no second validation pass and still no `onSubmit`. Save stays external;
   * the parent drives the §2 gate off this.
   *
   * Mount-time errors are reported too: RJSF validates hydrated formData on
   * mount (liveValidate) but only fires `onChange` on user edits, so an
   * already-invalid hydrated value would otherwise never reach the parent and
   * the §2/§9 fail-closed gate (which must block errors whose path does not
   * resolve) would not see it. `validateForm()` on mount is the form's OWN
   * validation — same validator, schema and code path as its `onChange` — so
   * this is not a parallel validation source.
   */
  onErrorsChange?: (errors: RJSFValidationError[]) => void;
}

export default function ControllerSpecForm({ value, onChange, onErrorsChange }: ControllerSpecFormProps) {
  const { data: openapiRoot, isLoading, error } = useOpenAPISchema();

  const schema = useMemo(() => {
    const raw = getControllerSpecSchema(openapiRoot);
    if (!raw) return undefined;
    // Remove excluded fields from the schema properties so they don't render at all
    const props = { ...(raw.properties as Record<string, unknown> || {}) };
    for (const key of EXCLUDED_FROM_TIER1) {
      delete props[key];
    }
    return { ...raw, properties: props } as RJSFSchema;
  }, [openapiRoot]);

  // The form's own ref, used to lift the errors RJSF already computed on mount.
  const formRef = useRef<Form>(null);

  // RJSF calls `onError` only when validation fails; passing a stable handler
  // (instead of the raw optional prop) also keeps `validateForm()` from falling
  // into its console.error branch when the parent does not listen.
  const handleOnError = useCallback(
    (errors: RJSFValidationError[]) => {
      onErrorsChange?.(errors);
    },
    [onErrorsChange],
  );

  // On mount (which happens again on every form hydration — SpecEditorCard keys
  // the form so it remounts with real data), re-run the form's own validation
  // so the already-computed errors reach onErrorsChange. RJSF computes them
  // internally on mount (liveValidate) but reports them only through its
  // onChange event, which a hydration mount never fires.
  useEffect(() => {
    formRef.current?.validateForm();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (isLoading) {
    return <div style={{ padding: 16, color: "var(--text-3)" }}>Loading schema...</div>;
  }
  if (error) {
    return <div style={{ padding: 16, color: "var(--warn-text)" }}>Failed to load schema: {String(error)}</div>;
  }
  if (!schema) {
    return <div style={{ padding: 16, color: "var(--warn-text)" }}>Schema not available</div>;
  }

  return (
    <VarroaForm
      ref={formRef}
      schema={schema}
      uiSchema={CONTROLLER_SPEC_UI_SCHEMA}
      validator={validator}
      formData={value}
      liveValidate
      onError={handleOnError}
      onChange={(e) => {
        onChange?.(e.formData as Record<string, unknown>);
        onErrorsChange?.(e.errors ?? []);
      }}
    >
      {/* No default submit button — save is handled externally */}
      <div />
    </VarroaForm>
  );
}
