package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// updateWriteAttempts bounds the read-merge-write rounds of an MCP update tool.
// Each round reads the object again, so a conflict merges onto the newer object.
const updateWriteAttempts = 3

// isConflictErr reports a conflict answer from the local store or from a
// member cluster over the bus.
func isConflictErr(err error) bool {
	if err == nil {
		return false
	}
	var be *api.BroodError
	return apierrors.IsConflict(err) || (errors.As(err, &be) && be.Code == bus.CodeConflict)
}

// updateTexts holds the texts that updateLocal puts in a tool error.
type updateTexts struct {
	tool     string // the tool name; the conflict text asks for a new call of it
	object   string // the object, for example "composed bundle ns/cb1"
	notFound string // the prefix of a read error, for example "composed bundle not found"
	failed   string // the prefix of a write error, for example "failed to update composed bundle"
}

// updateLocal reads one object from store, calls merge on it, and writes it with
// crdstore.ApplyAt, which sends the read's resourceVersion. A conflict starts a
// new round on a fresh read, at most updateWriteAttempts rounds. merge must
// derive every change from the object it receives and the call arguments,
// because each round calls it again. A merge error becomes the tool error text.
// On success the result is nil and the object is returned; otherwise the object
// is nil and the result holds the tool error.
func updateLocal[T any](ctx context.Context, store crdstore.Backend, name, namespace string, merge func(obj *T) error, texts updateTexts) (*T, *mcp.CallToolResult) {
	for attempt := 1; ; attempt++ {
		obj, err := crdstore.Get[T](ctx, store, name, namespace)
		if err != nil {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s: %v", texts.notFound, err))
		}
		if err := merge(obj); err != nil {
			return nil, mcp.NewToolResultError(err.Error())
		}
		err = crdstore.ApplyAt[T](ctx, store, obj)
		if err == nil {
			return obj, nil
		}
		if !isConflictErr(err) {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s: %v", texts.failed, err))
		}
		if attempt == updateWriteAttempts {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s changed during each of %d attempts: call %s again", texts.object, updateWriteAttempts, texts.tool))
		}
	}
}
