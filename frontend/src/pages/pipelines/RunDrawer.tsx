import { useMemo, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import type { RJSFSchema } from "@rjsf/utils";
import { customizeValidator } from "@rjsf/validator-ajv8";
import { VarroaForm } from "../../components/specform/VarroaFormTheme";
import { createRun } from "../../api/client";
import { uuidv7 } from "../../lib/uuidv7";
import type { PipelineParameter, PipelineParameterKind } from "../../types";
import styles from "./RunDrawer.module.css";

const validator = customizeValidator();

// The parameter kinds design D8's conversion table names. A kind outside
// this set renders as a plain string field instead of trusting a schema
// fragment the table never promised.
const KNOWN_KINDS = new Set<PipelineParameterKind>(["string", "boolean", "choice", "text", "password"]);

/** Converts one declared parameter into a JSON Schema property, per the
 * conversion table: string to string, boolean to boolean, choice to string
 * with an enumeration, text and password to string (the widget carries the
 * rest). A parameter type the table does not know renders as a plain string
 * field, always named by its own title. */
function paramProperty(param: PipelineParameter): Record<string, unknown> {
  const known = KNOWN_KINDS.has(param.kind) && param.schema && Object.keys(param.schema).length > 0;
  const base = known ? param.schema : { type: "string" };
  return { ...base, title: param.name };
}

function paramUiSchema(param: PipelineParameter): Record<string, unknown> {
  const widget = param.kind === "text" ? "textarea" : param.kind === "password" ? "password" : param.uiWidget;
  const ui: Record<string, unknown> = {};
  if (widget) ui["ui:widget"] = widget;
  if (param.description) ui["ui:description"] = param.description;
  return ui;
}

export interface RunDrawerProps {
  team: string;
  jobPath: string;
  parameters: PipelineParameter[];
  onClose: () => void;
  /** Called with the new run's id once the trigger request is accepted. */
  onSubmitted: (runId: string) => void;
}

export function RunDrawer({ team, jobPath, parameters, onClose, onSubmitted }: RunDrawerProps) {
  const [formData, setFormData] = useState<Record<string, unknown>>({});
  // One client token per drawer instance (design D3a): minted once on
  // mount, and reused on every submit from this drawer, so a repeated
  // submit — deliberate or a double click — carries the same token and the
  // BFF answers with one run.
  const [clientToken] = useState(() => uuidv7());

  const schema: RJSFSchema = useMemo(() => {
    const properties: Record<string, unknown> = {};
    for (const p of parameters) properties[p.name] = paramProperty(p);
    return { type: "object", properties } as RJSFSchema;
  }, [parameters]);

  const uiSchema = useMemo(() => {
    const ui: Record<string, unknown> = {};
    for (const p of parameters) ui[p.name] = paramUiSchema(p);
    return ui;
  }, [parameters]);

  const mutation = useMutation({
    mutationFn: () => {
      const stringParams: Record<string, string> = {};
      for (const [k, v] of Object.entries(formData)) {
        if (v !== undefined && v !== null) stringParams[k] = String(v);
      }
      return createRun(team, jobPath, { clientToken, parameters: stringParams });
    },
    onSuccess: (res) => onSubmitted(res.runId),
  });

  return (
    <div className={styles.overlay} onClick={onClose}>
      <div
        className={styles.drawer}
        role="dialog"
        aria-modal="true"
        aria-label={`Run ${jobPath}`}
        onClick={(e) => e.stopPropagation()}
      >
        <div className={styles.head}>
          <h2 className={styles.title}>Run {jobPath}</h2>
          <button type="button" className={styles.closeBtn} onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>

        <VarroaForm
          schema={schema}
          uiSchema={uiSchema}
          validator={validator}
          formData={formData}
          onChange={(e) => setFormData((e.formData ?? {}) as Record<string, unknown>)}
          onSubmit={() => mutation.mutate()}
        >
          <div className={styles.actions}>
            <button type="submit" className={styles.runBtn} disabled={mutation.isPending}>
              {mutation.isPending ? "Starting…" : "Run"}
            </button>
          </div>
        </VarroaForm>

        {mutation.isError && <p className={styles.error}>Varroa could not start this build.</p>}
      </div>
    </div>
  );
}
