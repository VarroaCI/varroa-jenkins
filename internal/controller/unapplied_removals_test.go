package controller

import (
	"errors"
	"reflect"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// TestApplyRemovalPaths pins the path collection shared by the BFF and Brood
// routes: exactly the null-valued keys of a spec patch, dot-separated and
// sorted, and the same ErrNullInList rejection the apply itself performs.
func TestApplyRemovalPaths(t *testing.T) {
	t.Run("collects and sorts paths at any depth", func(t *testing.T) {
		got, err := ApplyRemovalPaths(map[string]any{
			"hibernation": map[string]any{"activityIgnoreRegex": nil, "enabled": true},
			"version":     nil,
			"className":   "standard",
		})
		if err != nil {
			t.Fatalf("ApplyRemovalPaths: %v", err)
		}
		want := []string{"hibernation.activityIgnoreRegex", "version"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("empty patch yields no removals", func(t *testing.T) {
		got, err := ApplyRemovalPaths(nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("got %v err %v, want empty, nil", got, err)
		}
	})

	t.Run("null inside a list is rejected, matching apply", func(t *testing.T) {
		_, err := ApplyRemovalPaths(map[string]any{
			"resources": map[string]any{"claims": []any{nil}},
		})
		if !errors.Is(err, ErrNullInList) {
			t.Fatalf("err = %v, want ErrNullInList", err)
		}
	})
}

// TestUnappliedRemovals pins the shared presence check both routes use. The
// discriminating cases are: a removed field still on the applied object is
// reported; a field that actually went away is not; and presence is judged
// against MARSHALLED JSON (omitempty), so the local and Brood routes cannot
// diverge on a zero-valued field.
func TestUnappliedRemovals(t *testing.T) {
	t.Run("blocked removal is reported", func(t *testing.T) {
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Version: "2.479"}}
		got := UnappliedRemovals(applied, []string{"version"})
		want := []bus.UnappliedRemoval{{Field: "spec.version"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("nested blocked removal is reported", func(t *testing.T) {
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{
			Hibernation: &v1alpha1.HibernationSpec{ActivityIgnoreRegex: "cron"},
		}}
		got := UnappliedRemovals(applied, []string{"hibernation.activityIgnoreRegex"})
		want := []bus.UnappliedRemoval{{Field: "spec.hibernation.activityIgnoreRegex"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("removal that took effect is not reported", func(t *testing.T) {
		// The applied object has no version: the removal succeeded.
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{ClassName: "standard"}}
		got := UnappliedRemovals(applied, []string{"version"})
		if len(got) != 0 {
			t.Fatalf("got %+v, want no unapplied removals", got)
		}
	})

	t.Run("nested removal that took effect is not reported", func(t *testing.T) {
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{
			Hibernation: &v1alpha1.HibernationSpec{Enabled: true}, // no activityIgnoreRegex
		}}
		got := UnappliedRemovals(applied, []string{"hibernation.activityIgnoreRegex"})
		if len(got) != 0 {
			t.Fatalf("got %+v, want no unapplied removals", got)
		}
	})

	t.Run("presence is judged on marshalled JSON, not the Go struct (omitempty)", func(t *testing.T) {
		// Version "" IS present in the Go struct (a string field) but ABSENT in
		// marshalled JSON (omitempty). The TYPED wrapper below inherits that
		// omission — a retained zero value reads as absent here. Production
		// must therefore use the unstructured path (ApplyControllerSpecSSA's
		// res.Object), never this typed wrapper, so a field another manager
		// retained at its zero value is still reported. This case pins the
		// wrapper's limitation, not the seam's behavior.
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Version: ""}}
		got := UnappliedRemovals(applied, []string{"version"})
		if len(got) != 0 {
			t.Fatalf("got %+v, want no unapplied removals (empty string is omitted by omitempty)", got)
		}
	})

	t.Run("no requested removals reports nothing", func(t *testing.T) {
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Version: "2.479"}}
		got := UnappliedRemovals(applied, nil)
		if len(got) != 0 {
			t.Fatalf("got %+v, want no unapplied removals", got)
		}
	})

	t.Run("missing intermediate container reads as absent", func(t *testing.T) {
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{Version: "2.479"}}
		got := UnappliedRemovals(applied, []string{"hibernation.activityIgnoreRegex"})
		if len(got) != 0 {
			t.Fatalf("got %+v, want no unapplied removals", got)
		}
	})

	t.Run("free-form map key containing a dot is matched literally", func(t *testing.T) {
		// podAnnotations is a free-form map; its keys legitimately contain dots
		// (e.g. "example.com/keep"). The removal path must resolve the whole
		// remaining path as one literal key at the map leaf, not split it into
		// nested segments.
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{
			PodOverrides: &v1alpha1.PodOverrides{
				PodAnnotations: map[string]string{"example.com/keep": "v"},
			},
		}}
		got := UnappliedRemovals(applied, []string{"podOverrides.podAnnotations.example.com/keep"})
		want := []bus.UnappliedRemoval{{Field: "spec.podOverrides.podAnnotations.example.com/keep"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v (dotted map key must not be split)", got, want)
		}
	})

	t.Run("only requested removals are ever reported", func(t *testing.T) {
		// A field present on the applied object but never requested for removal
		// must not be reported.
		applied := &v1alpha1.Controller{Spec: v1alpha1.ControllerSpec{
			Version:   "2.479",
			ClassName: "standard",
		}}
		got := UnappliedRemovals(applied, []string{"className"})
		want := []bus.UnappliedRemoval{{Field: "spec.className"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v (version was never requested)", got, want)
		}
	})
}

// TestUnappliedRemovalsFromSpec pins the production presence check — the one
// ApplyControllerSpecSSA runs against the applied object's UNSTRUCTURED spec.
// Unlike the typed wrapper, the unstructured map distinguishes "present and
// zero-valued" from "absent", so a field another manager retained at its zero
// value is still reported.
func TestUnappliedRemovalsFromSpec(t *testing.T) {
	t.Run("retained zero value is reported", func(t *testing.T) {
		spec := map[string]any{
			"hibernation": map[string]any{"enabled": false},
		}
		got := UnappliedRemovalsFromSpec(spec, []string{"hibernation.enabled"})
		want := []bus.UnappliedRemoval{{Field: "spec.hibernation.enabled"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v (present-and-false must not read as absent)", got, want)
		}
	})

	t.Run("truly absent path is not reported", func(t *testing.T) {
		spec := map[string]any{"hibernation": map[string]any{}}
		got := UnappliedRemovalsFromSpec(spec, []string{"hibernation.enabled"})
		if len(got) != 0 {
			t.Fatalf("got %+v, want no unapplied removals (enabled is genuinely absent)", got)
		}
	})

	t.Run("no requested removals reports nothing", func(t *testing.T) {
		got := UnappliedRemovalsFromSpec(map[string]any{"version": "2.479"}, nil)
		if len(got) != 0 {
			t.Fatalf("got %+v, want no unapplied removals", got)
		}
	})
}
