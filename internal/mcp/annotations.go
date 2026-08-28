package mcp

import (
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// toolKind classifies a tool by its effect on the world. Clients use the
// annotations derived from it to decide when to interrupt a human for
// confirmation, so the classification is a user-facing contract, not metadata.
//
// It exists because mcp-go's NewTool defaults every annotation to the most
// alarming possible value — readOnlyHint:false, destructiveHint:true,
// idempotentHint:false, openWorldHint:true — and a tool that never sets them
// ships those defaults. That made all 64 tools indistinguishable: get_controller
// advertised itself exactly as destructively as delete_controller, so a client
// either prompts on every read or trusts none of the hints.
type toolKind int

const (
	// kindRead does not mutate anything: get_*, list_*, search, preview_*,
	// validate_*.
	kindRead toolKind = iota
	// kindCreate brings an object into existence. Destructive and idempotent,
	// because every create here goes through crdstore.Apply, which is a
	// full-object replace with upsert semantics: creating over an existing
	// object silently overwrites it rather than failing, and repeating the call
	// converges. Annotating it non-destructive would be a false safety signal —
	// create_group on an existing group replaces its member list.
	kindCreate
	// kindCreateStrict is a create that genuinely fails on conflict, so it is
	// additive-only: it cannot overwrite an existing object. It is the exception
	// here, applying only where the write reaches the operator's crdstore.Create
	// with no local Apply fallback. A tool with both paths gets kindCreate,
	// because in at least one topology it upserts.
	//
	// It is still idempotent. MCP defines idempotentHint as "no additional
	// effect on its environment" when repeated, not "returns the same response":
	// a retry conflicts and changes nothing, so a client that lost the first
	// response can safely retry. The REST habit of calling create
	// non-idempotent conflates erroring with mutating.
	kindCreateStrict
	// kindUpdate mutates an existing object. Also Apply-backed, so it replaces
	// the object wholesale: fields absent from the request are removed, not
	// preserved. That is a destructive update under MCP's definition, where
	// destructiveHint=false promises additive-only changes.
	kindUpdate
	// kindDelete removes an object. Destructive, but idempotent — deleting an
	// already-deleted object converges.
	kindDelete
	// kindAction triggers reconciliation, a restart, or a sync. Mutates cluster
	// state without destroying a declared object, and is not idempotent because
	// each call is a fresh occurrence.
	kindAction
	// kindProxy forwards a call to a Jenkins controller's own MCP server. The
	// effect is whatever the remote tool does, which this server cannot know, so
	// it is annotated conservatively and is the only kind that is genuinely
	// open-world.
	kindProxy
)

// annotationsFor maps a kind to its annotation set.
//
// openWorldHint is set explicitly on every kind, including the false cases.
// Leaving it unset does not mean false — the SDK default is true — so an
// omission would have 63 local-CRUD tools advertising that they reach out to
// the open internet.
func annotationsFor(kind toolKind) mcp.ToolAnnotation {
	readOnly, destructive, idempotent, openWorld := false, false, false, false
	switch kind {
	case kindRead:
		readOnly, idempotent = true, true
	case kindCreate:
		destructive, idempotent = true, true
	case kindCreateStrict:
		idempotent = true
	case kindUpdate:
		destructive, idempotent = true, true
	case kindDelete:
		destructive, idempotent = true, true
	case kindAction:
		// zero values: writes, non-destructive, non-idempotent
	case kindProxy:
		destructive, openWorld = true, true
	}
	return mcp.ToolAnnotation{
		ReadOnlyHint:    mcp.ToBoolPtr(readOnly),
		DestructiveHint: mcp.ToBoolPtr(destructive),
		IdempotentHint:  mcp.ToBoolPtr(idempotent),
		OpenWorldHint:   mcp.ToBoolPtr(openWorld),
	}
}

// mutates reports whether a tool of this kind changes cluster state and must
// therefore emit an activity event.
func (k toolKind) mutates() bool {
	switch k {
	case kindCreate, kindCreateStrict, kindUpdate, kindDelete, kindAction:
		return true
	default:
		return false
	}
}

// addTool registers a tool with annotations derived from its kind.
//
// kind is a required positional parameter rather than a functional option
// precisely so it cannot be forgotten: a new tool does not compile until its
// author has classified it. Register every tool through this helper rather than
// calling mcpServer.AddTool directly — annotationsCoverageTest enforces it.
func addTool(s *server.MCPServer, kind toolKind, t mcp.Tool, h server.ToolHandlerFunc) {
	title := t.Annotations.Title
	t.Annotations = annotationsFor(kind)
	t.Annotations.Title = title
	s.AddTool(t, h)
}
