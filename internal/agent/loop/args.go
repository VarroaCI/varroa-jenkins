package loop

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// CheckArguments checks the arguments of one call against the input schema of
// its tool. It answers ok when the schema accepts the arguments, or when the
// schema is empty or does not compile. A refusal names each leaf cause with
// the text that the MCP server writes for the same call. The rules and the
// text copy mcp-go input validation. A test in internal/mcp pins the match.
func CheckArguments(def agent.ToolDef, args json.RawMessage) (refusal string, ok bool) {
	if len(bytes.TrimSpace(def.Schema)) == 0 {
		return "", true
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(def.Schema))
	if err != nil {
		return "", true
	}
	url := "mem:///mcp-go/tools/" + def.Name + "/input-schema.json"
	c := jsonschema.NewCompiler()
	if err := c.AddResource(url, doc); err != nil {
		return "", true
	}
	sch, err := c.Compile(url)
	if err != nil {
		return "", true
	}
	var value any = map[string]any{}
	if t := bytes.TrimSpace(args); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		value, err = jsonschema.UnmarshalJSON(bytes.NewReader(t))
		if err != nil {
			return "invalid arguments: " + err.Error(), false
		}
	}
	err = sch.Validate(value)
	if err == nil {
		return "", true
	}
	const prefix = "input schema validation failed: "
	var verr *jsonschema.ValidationError
	if !errors.As(err, &verr) {
		return prefix + err.Error(), false
	}
	parts := validationLeaves(verr)
	if len(parts) == 0 {
		return prefix + verr.Error(), false
	}
	return prefix + strings.Join(parts, "; "), false
}

// validationLeaves returns one `<path>: <message>` line for each leaf cause of
// a validation error, in tree order.
func validationLeaves(v *jsonschema.ValidationError) []string {
	if len(v.Causes) == 0 {
		loc := ""
		for _, seg := range v.InstanceLocation {
			loc += "/" + strings.ReplaceAll(strings.ReplaceAll(seg, "~", "~0"), "/", "~1")
		}
		if loc == "" {
			loc = "<root>"
		}
		return []string{loc + ": " + v.ErrorKind.LocalizedString(message.NewPrinter(language.English))}
	}
	var out []string
	for _, c := range v.Causes {
		out = append(out, validationLeaves(c)...)
	}
	return out
}
