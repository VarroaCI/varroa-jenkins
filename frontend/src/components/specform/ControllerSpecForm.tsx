import { useMemo } from "react";
import { customizeValidator } from "@rjsf/validator-ajv8";
import type { RJSFSchema } from "@rjsf/utils";
import { useOpenAPISchema, getControllerSpecSchema } from "../../api/openapiSchema";
import { EXCLUDED_FROM_TIER1 } from "./excludedFields";
import { VarroaForm } from "./VarroaFormTheme";

const validator = customizeValidator();

interface ControllerSpecFormProps {
  value?: Record<string, unknown>;
  onChange?: (value: Record<string, unknown>) => void;
}

export default function ControllerSpecForm({ value, onChange }: ControllerSpecFormProps) {
  const { data: openapiRoot, isLoading, error } = useOpenAPISchema();

  const schema = useMemo(() => {
    const raw = getControllerSpecSchema(openapiRoot);
    if (!raw) return undefined;
    // Remove excluded fields from the schema properties so they don't render at all
    const props = { ...(raw.properties as Record<string, unknown> || {}) };
    for (const key of EXCLUDED_FROM_TIER1) {
      delete props[key];
    }
    // namespace is set at creation and cannot be changed afterward — render
    // it read-only instead of letting the generated form imply it's editable.
    if (props.namespace && typeof props.namespace === "object") {
      const ns = props.namespace as Record<string, unknown>;
      const baseDescription = typeof ns.description === "string" ? ns.description : "";
      props.namespace = {
        ...ns,
        readOnly: true,
        description: `${baseDescription ? `${baseDescription} ` : ""}Immutable after creation.`,
      };
    }
    return { ...raw, properties: props } as RJSFSchema;
  }, [openapiRoot]);

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
      schema={schema}
      validator={validator}
      formData={value}
      onChange={(e) => onChange?.(e.formData as Record<string, unknown>)}
    >
      {/* No default submit button — save is handled externally */}
      <div />
    </VarroaForm>
  );
}
