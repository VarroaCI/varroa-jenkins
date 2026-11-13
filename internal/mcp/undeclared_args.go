package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// undeclaredArgHints holds a sentence that follows the refusal of one tool.
var undeclaredArgHints = map[string]string{
	"create_controller": controllerSpecFieldsHint,
	"update_controller": controllerSpecFieldsHint,
}

// refuseUndeclaredArgs wraps h. A call with a top-level argument that the input
// schema of t does not declare fails before h runs, so the call changes
// nothing. The schemas keep additionalProperties open, because mcp-go renders
// that schema error as a Go struct, the same text as a missing required argument.
func refuseUndeclaredArgs(t mcp.Tool, h server.ToolHandlerFunc) server.ToolHandlerFunc {
	declared := make([]string, 0, len(t.InputSchema.Properties))
	known := make(map[string]bool, len(t.InputSchema.Properties))
	for name := range t.InputSchema.Properties {
		declared = append(declared, name)
		known[name] = true
	}
	sort.Strings(declared)
	hint := undeclaredArgHints[t.Name]
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var unknown []string
		for name := range getArgs(req) {
			if !known[name] {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) == 0 {
			return h(ctx, req)
		}
		sort.Strings(unknown)
		return mcp.NewToolResultError(undeclaredArgsText(t.Name, unknown, declared, hint)), nil
	}
}

// undeclaredArgsText names each refused argument and every declared one.
func undeclaredArgsText(tool string, unknown, declared []string, hint string) string {
	quoted := make([]string, len(unknown))
	for i, name := range unknown {
		quoted[i] = strconv.Quote(name)
	}
	noun := "argument"
	if len(unknown) > 1 {
		noun = "arguments"
	}
	takes := "It takes no arguments."
	if len(declared) > 0 {
		takes = "It takes only these arguments: " + strings.Join(declared, ", ") + "."
	}
	text := fmt.Sprintf("%s does not take the %s %s. %s The call changed nothing.",
		tool, noun, strings.Join(quoted, ", "), takes)
	if hint != "" {
		text += " " + hint
	}
	return text
}
