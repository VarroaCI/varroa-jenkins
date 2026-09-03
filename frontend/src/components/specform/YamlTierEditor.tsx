import { useCallback, useEffect, useMemo, useRef } from "react";
import { EditorState } from "@codemirror/state";
import { EditorView, keymap, lineNumbers } from "@codemirror/view";
import { yaml } from "@codemirror/lang-yaml";
import { linter, Diagnostic } from "@codemirror/lint";
import { defaultKeymap } from "@codemirror/commands";
import { parse as parseYAML } from "yaml";
import Ajv, { ValidateFunction } from "ajv";

interface YamlTierEditorProps {
  value: string;
  onChange: (value: string) => void;
  jsonSchema?: Record<string, unknown>;
  onDebouncedChange?: (value: string) => void;
  onValidityChange?: (valid: boolean) => void;
}

const DEBOUNCE_MS = 600;

const ajv = new Ajv({ allErrors: true, strict: false });

let validateCache: { schema: string; fn: ValidateFunction } | null = null;

function getValidator(schema?: Record<string, unknown>): ValidateFunction | null {
  if (!schema) return null;
  const key = JSON.stringify(schema);
  if (validateCache?.schema === key) return validateCache.fn;
  try {
    const fn = ajv.compile(schema);
    validateCache = { schema: key, fn };
    return fn;
  } catch {
    return null;
  }
}

// getSchema is read per lint pass, not captured: the OpenAPI schema is fetched
// asynchronously and can arrive after the editor is created.
function yamlLinter(getSchema: () => Record<string, unknown> | undefined) {
  return (view: EditorView): Diagnostic[] => {
    const validator = getValidator(getSchema());
    const text = view.state.doc.toString();
    if (!text.trim()) return [];

    const diagnostics: Diagnostic[] = [];

    // (a) YAML parse errors
    try {
      const parsed = parseYAML(text);
      // (b) AJV validation when JSON schema is provided
      if (validator && parsed !== undefined && parsed !== null) {
        const valid = validator(parsed);
        if (!valid && validator.errors) {
          for (const err of validator.errors) {
            diagnostics.push({
              from: 0,
              to: 1,
              severity: "error",
              message: `${err.instancePath || "/"} ${err.message}`,
            });
          }
        }
      }
    } catch (e: unknown) {
      const msg = e instanceof Error ? e.message : String(e);
      diagnostics.push({
        from: 0,
        to: 1,
        severity: "error",
        message: `YAML parse error: ${msg}`,
      });
    }

    return diagnostics;
  };
}

export default function YamlTierEditor({
  value,
  onChange,
  jsonSchema,
  onDebouncedChange,
  onValidityChange,
}: YamlTierEditorProps) {
  const viewRef = useRef<EditorView | null>(null);
  const containerRef = useRef<HTMLDivElement | null>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // The editor is created once, so the schema reaches its update listener and
  // its linter through this ref rather than a captured value.
  const schemaRef = useRef(jsonSchema);
  schemaRef.current = jsonSchema;

  // Compared by value: the caller derives the schema on each render, so its
  // identity changes even when the schema itself has not.
  const schemaKey = useMemo(
    () => (jsonSchema ? JSON.stringify(jsonSchema) : ""),
    [jsonSchema],
  );

  // Report validity changes
  const emitValidity = useCallback(
    (v: boolean) => {
      onValidityChange?.(v);
    },
    [onValidityChange],
  );

  // Emit validity on mount and again whenever the schema arrives or changes.
  // An editor mounted before the schema resolves would otherwise validate
  // syntax only, leaving Save enabled on schema-invalid content until the API
  // rejects it. viewRef is null on the mount pass (the editor is created by a
  // later effect), so the prop supplies the text then and the live document
  // supplies it afterwards.
  useEffect(() => {
    const text = viewRef.current?.state.doc.toString() ?? value ?? "";
    emitValidity(text.trim() ? checkValidity(text, schemaRef.current) : true);
    // A schema change also has to re-lint: the linter reads the schema per
    // pass, but nothing has invalidated its last result.
    viewRef.current?.dispatch({});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [schemaKey]);

  // Create editor view
  useEffect(() => {
    if (!containerRef.current) return;

    const updateListener = EditorView.updateListener.of((update) => {
      if (update.docChanged) {
        const text = update.state.doc.toString();

        // Sync to parent
        onChange(text);

        // Debounced callback (preview)
        if (onDebouncedChange) {
          if (debounceRef.current) clearTimeout(debounceRef.current);
          debounceRef.current = setTimeout(() => {
            onDebouncedChange(text);
          }, DEBOUNCE_MS);
        }

        // Validity check
        const isValid = text.trim() ? checkValidity(text, schemaRef.current) : true;
        emitValidity(isValid);
      }
    });

    const startState = EditorState.create({
      doc: value ?? "",
      extensions: [
        lineNumbers(),
        yaml(),
        keymap.of(defaultKeymap),
        linter(yamlLinter(() => schemaRef.current)),
        updateListener,
        EditorView.theme({
          "&": { fontFamily: "var(--mono)", fontSize: "13px", height: "100%" },
          ".cm-scroller": { overflow: "auto" },
          ".cm-content": { caretColor: "var(--text)" },
          ".cm-cursor": { borderLeftColor: "var(--text)" },
          "&.cm-focused .cm-cursor": { borderLeftColor: "var(--accent)" },
          "&.cm-focused": { outline: "none" },
          ".cm-gutters": {
            backgroundColor: "var(--surface-2)",
            borderRight: "1px solid var(--border)",
            color: "var(--text-3)",
          },
          ".cm-activeLineGutter": { backgroundColor: "var(--accent-soft)" },
          ".cm-diagnostic": { borderLeft: "3px solid var(--bad)" },
          ".cm-diagnostic-error": { borderLeftColor: "var(--bad)" },
        }),
        EditorView.baseTheme({
          "&.cm-editor.cm-focused": { outline: "none" },
          ".cm-selectionBackground": { background: "var(--accent-soft) !important" },
          ".cm-activeLine": { backgroundColor: "transparent" },
        }),
      ],
    });

    const view = new EditorView({
      state: startState,
      parent: containerRef.current,
    });
    viewRef.current = view;

    return () => {
      view.destroy();
      viewRef.current = null;
      if (debounceRef.current) clearTimeout(debounceRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div
      ref={containerRef}
      style={{
        border: `1px solid var(--border)`,
        borderRadius: "var(--radius-sm)",
        overflow: "hidden",
        minHeight: 120,
        background: "var(--surface)",
      }}
    />
  );
}

function checkValidity(text: string, jsonSchema?: Record<string, unknown>): boolean {
  try {
    const parsed = parseYAML(text);
    if (jsonSchema) {
      const validator = getValidator(jsonSchema);
      if (validator && !validator(parsed)) return false;
    }
    return true;
  } catch {
    return false;
  }
}
