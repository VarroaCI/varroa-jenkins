package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/controller/sharding"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// reconcileTestRunner returns a command runner whose reconciler acts on
// store, and the GVR of the Controller kind for error injection.
func reconcileTestRunner(t *testing.T, store *crdstore.Fake) (*operatorCommandRunner, *controller.Reconciler, schema.GroupVersionResource) {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatalf("controller GVR: %v", err)
	}
	rec := controller.NewReconciler(noop.NewMeterProvider(), nil, nil, store, nil, nil, nil, nil)
	rec.Logger = slog.Default()
	return &operatorCommandRunner{cluster: "core", reconciler: rec, logger: slog.Default()}, rec, gvr
}

// callResponder sends data to the row named name and decodes the reply.
func callResponder(t *testing.T, r *operatorCommandRunner, name string, data []byte) bus.ReconcileResponse {
	t.Helper()
	for _, cr := range r.commandResponders(context.Background()) {
		if cr.name != name {
			continue
		}
		var out bus.ReconcileResponse
		if err := json.Unmarshal(cr.handler(data), &out); err != nil {
			t.Fatalf("decode %s reply: %v", name, err)
		}
		return out
	}
	t.Fatalf("no responder named %s", name)
	return bus.ReconcileResponse{}
}

func seededControllerStore() *crdstore.Fake {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "ctrl"}})
	return store
}

// TestReconcileResponder_RepliesTheStepResult pins that the reconcile row
// replies after the step: not_found for a missing controller, internal
// with the error text for a failed routing patch, and success otherwise.
func TestReconcileResponder_RepliesTheStepResult(t *testing.T) {
	t.Run("missing controller", func(t *testing.T) {
		r, _, _ := reconcileTestRunner(t, crdstore.NewFake())
		out := callResponder(t, r, "reconcile", mustJSON(t, bus.ReconcileRequest{Namespace: "team-a", Name: "ghost"}))
		if out.Code != bus.CodeNotFound || out.Error == "" {
			t.Fatalf("reply = %+v, want code %q with the error text", out, bus.CodeNotFound)
		}
	})
	t.Run("owned controller", func(t *testing.T) {
		r, _, _ := reconcileTestRunner(t, seededControllerStore())
		out := callResponder(t, r, "reconcile", mustJSON(t, bus.ReconcileRequest{Namespace: "team-a", Name: "ctrl"}))
		if out != (bus.ReconcileResponse{}) {
			t.Fatalf("reply = %+v, want success", out)
		}
	})
	t.Run("routing patch fails", func(t *testing.T) {
		store := seededControllerStore()
		r, rec, gvr := reconcileTestRunner(t, store)
		// An empty shard set owns no controller, so the step patches the annotation.
		rec.SetShardOwnership(sharding.NewRing(8), sharding.NewShardSet())
		store.FailNext("patchmeta", gvr, errors.New("apiserver down"))
		out := callResponder(t, r, "reconcile", mustJSON(t, bus.ReconcileRequest{Namespace: "team-a", Name: "ctrl"}))
		if out.Code != bus.CodeInternal || !strings.Contains(out.Error, "apiserver down") {
			t.Fatalf("reply = %+v, want code %q with the patch error", out, bus.CodeInternal)
		}
	})
	t.Run("undecodable request", func(t *testing.T) {
		r, _, _ := reconcileTestRunner(t, crdstore.NewFake())
		out := callResponder(t, r, "reconcile", []byte("{"))
		if out.Code != bus.CodeInvalid {
			t.Fatalf("reply = %+v, want code %q", out, bus.CodeInvalid)
		}
	})
}

// TestReprovisionResponder_RepliesTheStepResult pins that the reprovision
// row replies with the result of the annotation patch.
func TestReprovisionResponder_RepliesTheStepResult(t *testing.T) {
	t.Run("missing controller", func(t *testing.T) {
		store := crdstore.NewFake()
		r, _, gvr := reconcileTestRunner(t, store)
		store.FailNext("patchmeta", gvr, apierrors.NewNotFound(schema.GroupResource{Group: gvr.Group, Resource: gvr.Resource}, "ghost"))
		out := callResponder(t, r, "reprovision", mustJSON(t, bus.ReconcileRequest{Namespace: "team-a", Name: "ghost"}))
		if out.Code != bus.CodeNotFound || out.Error == "" {
			t.Fatalf("reply = %+v, want code %q with the error text", out, bus.CodeNotFound)
		}
	})
	t.Run("patch fails", func(t *testing.T) {
		store := seededControllerStore()
		r, _, gvr := reconcileTestRunner(t, store)
		store.FailNext("patchmeta", gvr, errors.New("apiserver down"))
		out := callResponder(t, r, "reprovision", mustJSON(t, bus.ReconcileRequest{Namespace: "team-a", Name: "ctrl"}))
		if out.Code != bus.CodeInternal || !strings.Contains(out.Error, "apiserver down") {
			t.Fatalf("reply = %+v, want code %q with the patch error", out, bus.CodeInternal)
		}
	})
	t.Run("patch succeeds", func(t *testing.T) {
		store := seededControllerStore()
		r, _, gvr := reconcileTestRunner(t, store)
		out := callResponder(t, r, "reprovision", mustJSON(t, bus.ReconcileRequest{Namespace: "team-a", Name: "ctrl"}))
		if out != (bus.ReconcileResponse{}) {
			t.Fatalf("reply = %+v, want success", out)
		}
		if n := len(store.MetaPatches(gvr)); n != 1 {
			t.Fatalf("meta patches = %d, want 1", n)
		}
	})
}
