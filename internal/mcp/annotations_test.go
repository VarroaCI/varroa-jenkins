package mcp

import (
	"encoding/json"
	"sort"
	"testing"
)

// expectedToolKinds is the regression lock for tool annotations.
//
// Every registered tool must appear here, and its live annotations must match
// the kind claimed. The table is exhaustive on purpose: a tool added without an
// entry fails this test, which is the only thing standing between a new tool and
// silently shipping mcp-go's defaults (destructive, open-world, non-idempotent).
//
// Adding a tool means adding a line here. That is the intended cost.
var expectedToolKinds = map[string]toolKind{
	// Controllers
	"list_controllers":     kindRead,
	"get_controller":       kindRead,
	"get_controller_logs":  kindRead,
	"create_controller":    kindCreate,
	"update_controller":    kindUpdate,
	"delete_controller":    kindDelete,
	"reconcile_controller": kindAction,
	"restart_controller":   kindAction,
	"hibernate_controller": kindAction,
	"wake_controller":      kindAction,

	// VarroaRoles
	"list_varroa_roles":  kindRead,
	"get_varroa_role":    kindRead,
	"create_varroa_role": kindCreate,
	"update_varroa_role": kindUpdate,
	"delete_varroa_role": kindDelete,

	// VarroaRoleBindings
	"list_varroa_role_bindings":  kindRead,
	"get_varroa_role_binding":    kindRead,
	"create_varroa_role_binding": kindCreate,
	"update_varroa_role_binding": kindUpdate,
	"delete_varroa_role_binding": kindDelete,

	// JenkinsRoles
	"list_jenkins_roles":  kindRead,
	"get_jenkins_role":    kindRead,
	"create_jenkins_role": kindCreate,
	"update_jenkins_role": kindUpdate,
	"delete_jenkins_role": kindDelete,

	// JenkinsRoleBindings
	"list_jenkins_role_bindings":  kindRead,
	"get_jenkins_role_binding":    kindRead,
	"create_jenkins_role_binding": kindCreate,
	"update_jenkins_role_binding": kindUpdate,
	"delete_jenkins_role_binding": kindDelete,

	// CatalogSources
	"list_catalog_sources":  kindRead,
	"get_catalog_source":    kindRead,
	"create_catalog_source": kindCreate,
	"update_catalog_source": kindUpdate,
	"delete_catalog_source": kindDelete,
	"sync_catalog_source":   kindAction,

	// CatalogItems
	"list_catalog_items": kindRead,
	"get_catalog_item":   kindRead,

	// ComposedBundles
	"list_composed_bundles":    kindRead,
	"get_composed_bundle":      kindRead,
	"create_composed_bundle":   kindCreate,
	"update_composed_bundle":   kindUpdate,
	"delete_composed_bundle":   kindDelete,
	"validate_composed_bundle": kindRead,
	"preview_composed_bundle":  kindRead,

	// ProvisioningDefaults + JenkinsVersionProfiles
	"get_provisioning_defaults":      kindRead,
	"update_provisioning_defaults":   kindUpdate,
	"list_jenkins_version_profile":   kindRead,
	"get_jenkins_version_profile":    kindRead,
	"create_jenkins_version_profile": kindCreateStrict,
	"update_jenkins_version_profile": kindUpdate,
	"delete_jenkins_version_profile": kindDelete,
	"list_version_candidates":        kindRead,
	"get_version_candidate":          kindRead,
	"promote_version_candidate":      kindAction,

	// Users
	"list_users":  kindRead,
	"get_user":    kindRead,
	"create_user": kindCreate,
	"update_user": kindUpdate,
	"delete_user": kindDelete,

	// Groups
	"list_groups":  kindRead,
	"create_group": kindCreate,
	"delete_group": kindDelete,

	// Activity, search, identity
	"list_activity":      kindRead,
	"search":             kindRead,
	"get_me":             kindRead,
	"get_my_permissions": kindRead,

	// Jenkins MCP proxy
	"list_jenkins_controllers": kindRead,
	"call_jenkins_tool":        kindProxy,
}

// listedTool is the subset of a tools/list entry this test cares about.
type listedTool struct {
	Name        string          `json:"name"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
		OpenWorldHint   *bool `json:"openWorldHint"`
	} `json:"annotations"`
	OutputSchema json.RawMessage `json:"outputSchema"`
}

func liveTools(t *testing.T) []listedTool {
	t.Helper()
	handler := NewHandler(newTestDeps())
	resp := mcpRequest(t, handler, "tools/list", map[string]interface{}{}, nil)
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal tools/list: %v", err)
	}
	var payload struct {
		Tools []listedTool `json:"tools"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	if len(payload.Tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
	return payload.Tools
}

// TestEveryToolIsClassified fails when a tool is registered without a table
// entry, or when the table names a tool that no longer exists.
func TestEveryToolIsClassified(t *testing.T) {
	live := liveTools(t)

	seen := map[string]bool{}
	var unclassified []string
	for _, tool := range live {
		seen[tool.Name] = true
		if _, ok := expectedToolKinds[tool.Name]; !ok {
			unclassified = append(unclassified, tool.Name)
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("tools registered without a kind in expectedToolKinds: %v\n"+
			"Add each to the table and register it via addTool, or it ships mcp-go's "+
			"defaults (destructive, open-world).", unclassified)
	}

	var stale []string
	for name := range expectedToolKinds {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("expectedToolKinds names tools that are not registered: %v", stale)
	}

	if len(live) != len(expectedToolKinds) {
		t.Errorf("tool count = %d, table size = %d", len(live), len(expectedToolKinds))
	}
}

// TestToolAnnotationsMatchKind is the assertion that actually protects users:
// a client deciding whether to prompt reads these four booleans.
func TestToolAnnotationsMatchKind(t *testing.T) {
	deref := func(b *bool) bool { return b != nil && *b }

	for _, tool := range liveTools(t) {
		kind, ok := expectedToolKinds[tool.Name]
		if !ok {
			continue // reported by TestEveryToolIsClassified
		}
		want := annotationsFor(kind)
		got := tool.Annotations

		for _, c := range []struct {
			field     string
			got, want bool
		}{
			{"readOnlyHint", deref(got.ReadOnlyHint), deref(want.ReadOnlyHint)},
			{"destructiveHint", deref(got.DestructiveHint), deref(want.DestructiveHint)},
			{"idempotentHint", deref(got.IdempotentHint), deref(want.IdempotentHint)},
			{"openWorldHint", deref(got.OpenWorldHint), deref(want.OpenWorldHint)},
		} {
			if c.got != c.want {
				t.Errorf("%s: %s = %v, want %v", tool.Name, c.field, c.got, c.want)
			}
		}
	}
}

// TestWriteToolsAreAdvertisedDestructive pins the policy rather than its
// self-consistency: TestToolAnnotationsMatchKind compares live annotations
// against annotationsFor, so both sides move together and a wrong policy passes.
//
// Almost every create and update in this package is backed by crdstore.Apply, a
// full-object replace with upsert semantics — a create silently overwrites an
// existing object, and an update drops fields absent from the request. Claiming
// destructiveHint=false there tells a client it is safe to run these without
// asking, which is the opposite of true.
func TestWriteToolsAreAdvertisedDestructive(t *testing.T) {
	for _, tool := range liveTools(t) {
		switch expectedToolKinds[tool.Name] {
		case kindCreate, kindUpdate, kindDelete:
		default:
			continue
		}
		if tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint {
			t.Errorf("%s replaces or removes declared state but advertises destructiveHint=false", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint == nil || *tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is a write tool but advertises readOnlyHint=true", tool.Name)
		}
	}
}

// kindCreateStrict is the documented exception, so it gets its own assertion
// rather than an exemption: a tool claiming additive-only semantics must
// actually fail on conflict, and reclassifying an Apply-backed create into this
// kind to quiet the test above would be a regression, not a fix.
func TestStrictCreatesAreAdditiveButStillIdempotent(t *testing.T) {
	found := 0
	for _, tool := range liveTools(t) {
		if expectedToolKinds[tool.Name] != kindCreateStrict {
			continue
		}
		found++
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("%s is a strict create and must advertise destructiveHint=false", tool.Name)
		}
		// MCP idempotence is about additional environmental effect, not about
		// the response: a retry conflicts and mutates nothing, so a client that
		// lost the first response may safely retry.
		if tool.Annotations.IdempotentHint == nil || !*tool.Annotations.IdempotentHint {
			t.Errorf("%s changes nothing when retried, so it must advertise idempotentHint=true", tool.Name)
		}
	}
	// Only create_jenkins_version_profile reaches the operator's crdstore.Create
	// with no Apply fallback. If this count grows, verify the new tool really
	// cannot upsert before accepting it.
	if found != 1 {
		t.Errorf("expected exactly 1 strict-create tool, found %d — verify each cannot upsert", found)
	}
}

// TestReadOnlyToolsAreNotAdvertisedDestructive states the user-visible outcome
// directly, independent of the table: nothing a caller can only read from may
// tell a client it destroys data.
func TestReadOnlyToolsAreNotAdvertisedDestructive(t *testing.T) {
	for _, tool := range liveTools(t) {
		if expectedToolKinds[tool.Name] != kindRead {
			continue
		}
		if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Errorf("%s is read-only but advertises destructiveHint=true", tool.Name)
		}
		if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is read-only but does not advertise readOnlyHint=true", tool.Name)
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s advertises openWorldHint=true; only the Jenkins proxy is open-world", tool.Name)
		}
	}
}
