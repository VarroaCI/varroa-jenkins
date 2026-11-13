package mcp

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestConfigWriteTools_LocalWriteGoesThroughTheLocalOperator checks the 12
// writes without cluster: four updates, four creates, and four deletes. Each
// makes one write through the local operator and none on the member, changes
// the local store, and emits one event without a cluster.
func TestConfigWriteTools_LocalWriteGoesThroughTheLocalOperator(t *testing.T) {
	check := func(t *testing.T, res toolResult, member *memberConfig, sink *recordingSink) {
		t.Helper()
		if res.IsError {
			t.Fatalf("write failed: %s", contentText(res))
		}
		if len(member.localWrites) != 1 || len(member.updates) != 0 {
			t.Errorf("local writes = %v, member writes = %v, want one local write only", member.localWrites, member.updates)
		}
		if len(sink.events) != 1 || sink.events[0].Cluster != "" {
			t.Errorf("events = %+v, want one event without a cluster", sink.events)
		}
	}
	for _, kind := range configKinds() {
		t.Run("update "+kind.name, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(t, local, "local-desc")
			res := callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, kind.setArgs))
			check(t, res, member, sink)
			if got := kind.read(t, local); got != "changed" {
				t.Errorf("local value = %q, want changed", got)
			}
		})
	}
	for _, kind := range writeKinds() {
		t.Run("create "+kind.label, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			res := callToolAs(t, deps, mcpAdminClaims, kind.create, kind.args(nil))
			check(t, res, member, sink)
			if !kind.exists(local, "obj", kind.namespace) {
				t.Error("create must write the local object")
			}
		})
		t.Run("delete "+kind.label, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(local, "obj", kind.namespace)
			res := callToolAs(t, deps, mcpAdminClaims, kind.del, kind.delArgs(nil))
			check(t, res, member, sink)
			if kind.exists(local, "obj", kind.namespace) {
				t.Error("delete must remove the local object")
			}
		})
	}
}

// TestConfigWriteTools_LocalOperatorDownChangesNothing checks that a write
// without cluster fails when the local operator does not answer. The local
// object does not change, and no event is emitted.
func TestConfigWriteTools_LocalOperatorDownChangesNothing(t *testing.T) {
	failed := func(t *testing.T, res toolResult) {
		t.Helper()
		if !res.IsError || !strings.Contains(contentText(res), "cluster core unreachable") {
			t.Errorf("result = %q, want a failure that names core", contentText(res))
		}
	}
	for _, kind := range configKinds() {
		t.Run("update "+kind.name, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(t, local, "local-desc")
			member.localDown = true
			failed(t, callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, kind.setArgs)))
			if kind.read(t, local) != "local-desc" || len(sink.events) != 0 {
				t.Error("a down local operator changed the local object or emitted an event")
			}
		})
	}
	for _, kind := range writeKinds() {
		t.Run("create "+kind.label, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			member.localDown = true
			failed(t, callToolAs(t, deps, mcpAdminClaims, kind.create, kind.args(nil)))
			if kind.exists(local, "obj", kind.namespace) || len(sink.events) != 0 {
				t.Error("a down local operator created the object or emitted an event")
			}
		})
		t.Run("delete "+kind.label, func(t *testing.T) {
			deps, local, member, sink := memberDeps(t)
			kind.seed(local, "obj", kind.namespace)
			member.localDown = true
			failed(t, callToolAs(t, deps, mcpAdminClaims, kind.del, kind.delArgs(nil)))
			if !kind.exists(local, "obj", kind.namespace) || len(sink.events) != 0 {
				t.Error("a down local operator removed the object or emitted an event")
			}
		})
	}
}

// TestConfigWriteTools_NoConfigBroodRefusesTheWrite checks that each write
// without cluster refuses the call on a server without a config brood. The
// local store does not change.
func TestConfigWriteTools_NoConfigBroodRefusesTheWrite(t *testing.T) {
	refused := func(t *testing.T, res toolResult) {
		t.Helper()
		if !res.IsError || contentText(res) != "config brood not configured" {
			t.Errorf("result = %q, want the config brood refusal", contentText(res))
		}
	}
	for _, kind := range configKinds() {
		t.Run("update "+kind.name, func(t *testing.T) {
			deps := adminDeps()
			local := deps.Store.(*crdstore.Fake)
			kind.seed(t, local, "local-desc")
			refused(t, callToolAs(t, deps, mcpAdminClaims, kind.update, withArgs(kind.args, kind.setArgs)))
			if kind.read(t, local) != "local-desc" {
				t.Error("the refused update changed the local object")
			}
		})
	}
	for _, kind := range writeKinds() {
		t.Run("create "+kind.label, func(t *testing.T) {
			deps := adminDeps()
			refused(t, callToolAs(t, deps, mcpAdminClaims, kind.create, kind.args(nil)))
			if kind.exists(deps.Store.(*crdstore.Fake), "obj", kind.namespace) {
				t.Error("the refused create wrote the object")
			}
		})
		t.Run("delete "+kind.label, func(t *testing.T) {
			deps := adminDeps()
			local := deps.Store.(*crdstore.Fake)
			kind.seed(local, "obj", kind.namespace)
			refused(t, callToolAs(t, deps, mcpAdminClaims, kind.del, kind.delArgs(nil)))
			if !kind.exists(local, "obj", kind.namespace) {
				t.Error("the refused delete removed the object")
			}
		})
	}
}
