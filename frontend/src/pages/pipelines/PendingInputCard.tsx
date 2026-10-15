import { useState } from "react";
import { Button } from "../../components/Button";
import { Card } from "../../components/Card";
import type { RunInput, RunInputParameter } from "../../types";
import styles from "./PendingInputCard.module.css";

interface PendingInputCardProps {
  inputs: RunInput[];
  canWrite: boolean;
  /** The input a write request is in flight for. That entry's controls are
   * disabled. */
  busyInputId?: string | null;
  onProceed: (inputId: string, parameters: Record<string, string>) => void;
  onAbort: (inputId: string) => void;
}

/** The declared value of every parameter of one input, keyed by name. */
function seed(parameters: RunInputParameter[]): Record<string, string> {
  const values: Record<string, string> = {};
  for (const parameter of parameters) {
    values[parameter.name] = parameter.defaultValue ?? "";
  }
  return values;
}

interface ParameterControlProps {
  inputId: string;
  parameter: RunInputParameter;
  value: string;
  disabled: boolean;
  onChange: (value: string) => void;
}

/** One parameter's control. A declared choice list wins over the type, and
 * the type decides every other control. */
function ParameterControl({
  inputId,
  parameter,
  value,
  disabled,
  onChange,
}: ParameterControlProps) {
  const id = `input-${inputId}-${parameter.name}`;
  const choices = parameter.choices ?? [];
  if (choices.length > 0) {
    return (
      <select id={id} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
        {choices.map((choice) => (
          <option key={choice} value={choice}>
            {choice}
          </option>
        ))}
      </select>
    );
  }
  if (parameter.type === "BooleanParameterDefinition") {
    return (
      <input
        id={id}
        type="checkbox"
        checked={value === "true"}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked ? "true" : "false")}
      />
    );
  }
  if (parameter.type === "PasswordParameterDefinition") {
    return (
      <input
        id={id}
        type="password"
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
      />
    );
  }
  if (parameter.type === "FileParameterDefinition") {
    return <input id={id} type="text" value={value} readOnly disabled />;
  }
  return (
    <input
      id={id}
      type="text"
      value={value}
      disabled={disabled}
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

/** The run's pending input steps. One entry per step, and one control per
 * declared parameter. */
export function PendingInputCard({
  inputs,
  canWrite,
  busyInputId,
  onProceed,
  onAbort,
}: PendingInputCardProps) {
  const [edits, setEdits] = useState<Record<string, Record<string, string>>>({});

  const setValue = (inputId: string, name: string, value: string) => {
    setEdits((current) => ({
      ...current,
      [inputId]: { ...(current[inputId] ?? {}), [name]: value },
    }));
  };

  return (
    <Card title="Pending input">
      {inputs.map((input) => {
        const parameters = input.parameters ?? [];
        // An edited value wins over the declared default, so an untouched
        // control re-seeds after a refetch and an edited one holds its value.
        // The request carries the edits alone: the plugin applies the
        // declared default for every omitted name, and a sensitive default
        // never reaches this page, so a seeded value must not go back.
        const values = { ...seed(parameters), ...(edits[input.id] ?? {}) };
        const busy = busyInputId === input.id;
        const locked = !canWrite || busy;
        return (
          <div key={input.id} className={styles.entry}>
            <p className={styles.message}>{input.message || "This build waits for an answer."}</p>
            {input.submitter && (
              <p className={styles.submitter}>Only {input.submitter} may answer this input.</p>
            )}
            {parameters.length > 0 && (
              <div className={styles.form}>
                {parameters.map((parameter) => (
                  <div key={parameter.name} className={styles.field}>
                    <label htmlFor={`input-${input.id}-${parameter.name}`}>{parameter.name}</label>
                    <ParameterControl
                      inputId={input.id}
                      parameter={parameter}
                      value={values[parameter.name] ?? ""}
                      disabled={locked}
                      onChange={(value) => setValue(input.id, parameter.name, value)}
                    />
                    {parameter.type === "FileParameterDefinition" && (
                      <span className={styles.note}>
                        A file parameter cannot be set from this page. The step uses its own value.
                      </span>
                    )}
                  </div>
                ))}
              </div>
            )}
            <div className={styles.actions}>
              {canWrite && (
                <>
                  <Button
                    type="button"
                    variant="primary"
                    disabled={busy}
                    onClick={() => onProceed(input.id, edits[input.id] ?? {})}
                  >
                    {input.ok || "Proceed"}
                  </Button>
                  <Button type="button" disabled={busy} onClick={() => onAbort(input.id)}>
                    Abort
                  </Button>
                </>
              )}
            </div>
          </div>
        );
      })}
    </Card>
  );
}
