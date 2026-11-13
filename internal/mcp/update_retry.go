package mcp

import (
	"context"
	"encoding/json"
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

// updateTexts holds the texts that updateObject puts in a tool error.
type updateTexts struct {
	tool     string // the tool name; the conflict text asks for a new call of it
	object   string // the object, for example "composed bundle ns/cb1"
	notFound string // the prefix of a not-found read error, for example "composed bundle not found"
	failed   string // the prefix of a write error, for example "failed to update composed bundle"
}

// configObject reads and writes one config object on one cluster.
type configObject[T any] struct {
	get   func(ctx context.Context) (*T, error)
	write func(ctx context.Context, obj *T) (*T, error)
}

// storeObject addresses one object in store. The write sends the resourceVersion of obj.
func storeObject[T any](store crdstore.Backend, name, namespace string) configObject[T] {
	return configObject[T]{
		get: func(ctx context.Context) (*T, error) { return crdstore.Get[T](ctx, store, name, namespace) },
		write: func(ctx context.Context, obj *T) (*T, error) {
			if err := crdstore.ApplyAt[T](ctx, store, obj); err != nil {
				return nil, err
			}
			return obj, nil
		},
	}
}

// broodObject addresses one object on a member cluster through two ConfigBrood
// methods of its kind. The write body carries the resourceVersion of the read,
// so the member operator answers conflict for a stale read.
func broodObject[T any](get func(ctx context.Context) (json.RawMessage, error), update func(ctx context.Context, obj json.RawMessage) (json.RawMessage, error)) configObject[T] {
	return configObject[T]{
		get: func(ctx context.Context) (*T, error) {
			raw, err := get(ctx)
			if err != nil {
				return nil, err
			}
			var obj T
			if err := json.Unmarshal(raw, &obj); err != nil {
				return nil, fmt.Errorf("decode the stored object: %w", err)
			}
			return &obj, nil
		},
		write: func(ctx context.Context, obj *T) (*T, error) {
			if update == nil {
				return nil, errors.New("the object is read-only on this cluster")
			}
			body, err := json.Marshal(obj)
			if err != nil {
				return nil, fmt.Errorf("encode the object: %w", err)
			}
			raw, err := update(ctx, body)
			if err != nil {
				return nil, err
			}
			var written T
			if err := json.Unmarshal(raw, &written); err != nil {
				return nil, fmt.Errorf("decode the written object: %w", err)
			}
			return &written, nil
		},
	}
}

// readErrorText words a failed read. Only a not-found answer says the object is missing.
func readErrorText(notFound, object string, err error) string {
	if isNotFoundErr(err) {
		return notFound + ": " + err.Error()
	}
	return "failed to read " + object + ": " + err.Error()
}

// updateObject reads one object through target, calls merge on it, and writes
// it back, so a write sends the read's resourceVersion. A conflict starts a new
// round on a fresh read, at most updateWriteAttempts rounds. merge must derive
// every change from the object it receives and the call arguments, because each
// round calls it again. A merge error becomes the tool error text. On success
// the result is nil and the object that target wrote is returned; otherwise the
// object is nil and the result holds the tool error.
func updateObject[T any](ctx context.Context, target configObject[T], merge func(obj *T) error, texts updateTexts) (*T, *mcp.CallToolResult) {
	for attempt := 1; ; attempt++ {
		obj, err := target.get(ctx)
		if err != nil {
			return nil, mcp.NewToolResultError(readErrorText(texts.notFound, texts.object, err))
		}
		if err := merge(obj); err != nil {
			return nil, mcp.NewToolResultError(err.Error())
		}
		written, err := target.write(ctx, obj)
		if err == nil {
			return written, nil
		}
		if !isConflictErr(err) {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s: %v", texts.failed, err))
		}
		if attempt == updateWriteAttempts {
			return nil, mcp.NewToolResultError(fmt.Sprintf("%s changed during each of %d attempts: call %s again", texts.object, updateWriteAttempts, texts.tool))
		}
	}
}

// updateLocal is updateObject on the local store.
func updateLocal[T any](ctx context.Context, store crdstore.Backend, name, namespace string, merge func(obj *T) error, texts updateTexts) (*T, *mcp.CallToolResult) {
	return updateObject(ctx, storeObject[T](store, name, namespace), merge, texts)
}
