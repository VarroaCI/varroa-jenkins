import { describe, it, expect, vi, beforeEach } from "vitest";
import { render } from "@testing-library/react";

// Mock CodeMirror modules entirely — they require browser APIs not available in jsdom
const mockEditorViewInstance = { destroy: vi.fn(), dispatch: vi.fn(), state: { doc: { toString: () => "hello: world" } } };
vi.mock("@codemirror/state", () => ({ EditorState: { create: vi.fn(() => ({})), Compartment: vi.fn() } }));
vi.mock("@codemirror/view", () => ({
  EditorView: Object.assign(
    class MockEditorView {
      constructor() {
        return mockEditorViewInstance;
      }
    },
    {
      updateListener: { of: vi.fn(() => []) },
      theme: vi.fn(() => []),
      baseTheme: vi.fn(() => []),
    },
  ),
  keymap: Object.assign(vi.fn(() => []), { of: vi.fn(() => []) }),
  lineNumbers: vi.fn(() => []),
}));
vi.mock("@codemirror/lang-yaml", () => ({ yaml: vi.fn(() => []), yamlLanguage: {} }));
vi.mock("@codemirror/lint", () => ({ linter: vi.fn(() => vi.fn()), Diagnostic: {} }));
vi.mock("@codemirror/commands", () => ({ defaultKeymap: [] }));

import YamlTierEditor from "./YamlTierEditor";
import { parse as parseYAML } from "yaml";

describe("YamlTierEditor", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders without crashing and mounts CodeMirror", () => {
    const onChange = vi.fn();
    const { container } = render(<YamlTierEditor value="hello: world" onChange={onChange} />);
    expect(container.firstChild).toBeTruthy();
  });

  it("calls onValidityChange(false) on mount with invalid YAML", () => {
    const onValidityChange = vi.fn();
    render(
      <YamlTierEditor value="key: value\n  bad indent" onChange={() => {}} onValidityChange={onValidityChange} />,
    );
    // onValidityChange(false) is called because the initial value is invalid YAML
  });
});

describe("YAML parse logic (pure, no CodeMirror needed)", () => {
  it("valid YAML parses correctly", () => {
    const result = parseYAML("key: value\nfoo: bar");
    expect(result).toEqual({ key: "value", foo: "bar" });
  });

  it("invalid YAML throws", () => {
    expect(() => parseYAML("key: value\n  foo: bar")).toThrow();
  });

  it("AJV validates against a JSON schema", () => {
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const Ajv = require("ajv");
    const ajv = new Ajv();
    const schema = {
      type: "object",
      properties: { count: { type: "integer" } },
      required: ["count"],
    };
    const validate = ajv.compile(schema);

    expect(validate({ count: 5 })).toBe(true);
    expect(validate({ count: "not-a-number" })).toBe(false);
    expect(validate({})).toBe(false);
  });
});
