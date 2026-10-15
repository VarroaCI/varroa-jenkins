package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// poolGiteaCoreSet pins the gitea plugin that a pool Gitea server needs.
func poolGiteaCoreSet() []pluginlock.PluginEntry {
	return []pluginlock.PluginEntry{{ArtifactID: "gitea", Version: "283.v14b_179495971"}}
}

// seedPoolGiteaPool registers a pool that declares servers and labels cr
// with it.
func seedPoolGiteaPool(client *testClient, cr *v1alpha1.Controller, servers []v1alpha1.CellPoolGiteaServer) {
	crdstore.MustSeed(client.store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef:   "lts-2-555",
			GiteaServers: servers,
		},
	})
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
}

// jcascGiteaServers parses jcasc and returns unclassified.giteaServers.servers.
// It fails the test when the key is missing.
func jcascGiteaServers(t *testing.T, jcasc string) []map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(jcasc), &doc); err != nil {
		t.Fatalf("parse jcasc: %v", err)
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	gitea, _ := unclassified["giteaServers"].(map[string]any)
	raw, ok := gitea["servers"].([]any)
	if !ok {
		t.Fatalf("jcasc has no unclassified.giteaServers.servers list:\n%s", jcasc)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestRenderPoolGiteaServers_RendersEntriesWithHooksOff(t *testing.T) {
	set := renderPoolGiteaServers([]v1alpha1.CellPoolGiteaServer{
		{ServerURL: "https://git.example.org", DisplayName: "Example", AliasURL: "https://forge.example.org"},
		{ServerURL: "https://forgejo.example.net"},
	}, poolGiteaCoreSet())

	if set.Reason != ReasonPoolGiteaServersRendered || len(set.Faults) != 0 {
		t.Fatalf("reason=%q faults=%v, want Rendered and no faults", set.Reason, set.Faults)
	}
	if len(set.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(set.Entries))
	}
	first := set.Entries[0].(map[string]any)
	want := map[string]any{
		"serverUrl":   "https://git.example.org",
		"manageHooks": false,
		"displayName": "Example",
		"aliasUrl":    "https://forge.example.org",
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first entry = %v, want %v", first, want)
	}
	second := set.Entries[1].(map[string]any)
	if _, ok := second["displayName"]; ok {
		t.Error("an empty displayName must be omitted")
	}
	if _, ok := second["aliasUrl"]; ok {
		t.Error("an empty aliasUrl must be omitted")
	}
	for _, e := range set.Entries {
		if _, ok := e.(map[string]any)["credentialsId"]; ok {
			t.Error("a pool Gitea server entry must carry no credentialsId")
		}
	}
}

func TestRenderPoolGiteaServers_TrimsTrailingSlash(t *testing.T) {
	set := renderPoolGiteaServers([]v1alpha1.CellPoolGiteaServer{
		{ServerURL: "https://git.example.org/", AliasURL: "https://forge.example.org/"},
	}, poolGiteaCoreSet())
	entry := set.Entries[0].(map[string]any)
	if entry["serverUrl"] != "https://git.example.org" {
		t.Errorf("serverUrl = %v, want the trailing slash removed", entry["serverUrl"])
	}
	if entry["aliasUrl"] != "https://forge.example.org" {
		t.Errorf("aliasUrl = %v, want the trailing slash removed", entry["aliasUrl"])
	}
}

func TestRenderPoolGiteaServers_PluginMissingRendersNothing(t *testing.T) {
	set := renderPoolGiteaServers([]v1alpha1.CellPoolGiteaServer{
		{ServerURL: "https://git.example.org"},
	}, nil)
	if set.Reason != ReasonPoolGiteaServerPluginMissing {
		t.Fatalf("reason = %q, want PluginMissing", set.Reason)
	}
	if len(set.Entries) != 0 {
		t.Fatalf("entries = %v, want none when the plugin is not pinned", set.Entries)
	}
}

func TestRenderPoolGiteaServers_RejectsVarToken(t *testing.T) {
	set := renderPoolGiteaServers([]v1alpha1.CellPoolGiteaServer{
		{ServerURL: "https://git.example.org/${ENV}"},
		{ServerURL: "https://ok.example.org"},
	}, poolGiteaCoreSet())
	if set.Reason != ReasonPoolGiteaServerInvalid {
		t.Fatalf("reason = %q, want ServerInvalid", set.Reason)
	}
	if len(set.Entries) != 1 || set.Entries[0].(map[string]any)["serverUrl"] != "https://ok.example.org" {
		t.Fatalf("entries = %v, want only the valid server", set.Entries)
	}
	if len(set.Faults) != 1 {
		t.Fatalf("faults = %v, want one", set.Faults)
	}
	if !strings.Contains(set.Faults[0], "field serverUrl") {
		t.Fatalf("fault = %q, want it to name the serverUrl field", set.Faults[0])
	}
}

func TestRenderPoolGiteaServers_NamesDisplayNameTokenField(t *testing.T) {
	set := renderPoolGiteaServers([]v1alpha1.CellPoolGiteaServer{
		{ServerURL: "https://git.example.org", DisplayName: "forge-${NAME}"},
	}, poolGiteaCoreSet())
	if set.Reason != ReasonPoolGiteaServerInvalid || len(set.Entries) != 0 {
		t.Fatalf("reason=%q entries=%v, want ServerInvalid and no entry", set.Reason, set.Entries)
	}
	if len(set.Faults) != 1 || !strings.Contains(set.Faults[0], "field displayName") {
		t.Fatalf("faults = %v, want one fault that names the displayName field", set.Faults)
	}
}

// TestNormalizeGiteaServerURL checks the comparison form against the rules
// of the plugin's server lookup.
func TestNormalizeGiteaServerURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "host case folds", in: "https://GIT.Example.ORG", want: "https://git.example.org"},
		{name: "path case kept", in: "https://git.example.org/Forge", want: "https://git.example.org/Forge"},
		{name: "https 443 dropped", in: "https://git.example.org:443", want: "https://git.example.org"},
		{name: "http 80 dropped", in: "http://git.example.org:80", want: "http://git.example.org"},
		{name: "non-default port kept", in: "https://git.example.org:8443", want: "https://git.example.org:8443"},
		{name: "trailing slash removed", in: "https://git.example.org/", want: "https://git.example.org"},
		{name: "space trimmed", in: "  https://git.example.org/forge/ ", want: "https://git.example.org/forge"},
		{name: "unparsable keeps text", in: "git.example.org/", want: "git.example.org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeGiteaServerURL(tt.in); got != tt.want {
				t.Fatalf("normalizeGiteaServerURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestRenderPoolGiteaServers_DedupesCaseSensitivePaths checks that two
// servers that differ only in path case both render, because the plugin
// treats the paths as different servers.
func TestRenderPoolGiteaServers_DedupesCaseSensitivePaths(t *testing.T) {
	set := renderPoolGiteaServers([]v1alpha1.CellPoolGiteaServer{
		{ServerURL: "https://git.example.org/Forge"},
		{ServerURL: "https://git.example.org/forge"},
	}, poolGiteaCoreSet())
	if len(set.Entries) != 2 {
		t.Fatalf("entries = %v, want both path spellings to render", set.Entries)
	}
}

// TestRenderPoolGiteaServers_DedupesNormalizedURLs checks that two spellings
// of one forge render once. The first spelling in spec order wins.
func TestRenderPoolGiteaServers_DedupesNormalizedURLs(t *testing.T) {
	set := renderPoolGiteaServers([]v1alpha1.CellPoolGiteaServer{
		{ServerURL: "https://git.example.org", DisplayName: "first"},
		{ServerURL: "https://GIT.example.org/", DisplayName: "second"},
		{ServerURL: "https://other.example.org"},
	}, poolGiteaCoreSet())
	if len(set.Entries) != 2 {
		t.Fatalf("entries = %v, want two after the normalized duplicate is dropped", set.Entries)
	}
	first := set.Entries[0].(map[string]any)
	if first["displayName"] != "first" {
		t.Fatalf("first entry = %v, want the first spelling to win", first)
	}
	if set.Entries[1].(map[string]any)["serverUrl"] != "https://other.example.org" {
		t.Fatalf("second entry = %v, want the other server", set.Entries[1])
	}
}

func TestInjectPoolGiteaServers_KeepsBundleServersAndAppends(t *testing.T) {
	jcasc := "unclassified:\n  giteaServers:\n    servers:\n      - serverUrl: https://bundle.example.org\n        manageHooks: false\n"
	entries := []any{map[string]any{"serverUrl": "https://pool.example.org", "manageHooks": false}}

	out, err := injectPoolGiteaServers(jcasc, entries)
	if err != nil {
		t.Fatalf("injectPoolGiteaServers: %v", err)
	}
	servers := jcascGiteaServers(t, out)
	if len(servers) != 2 {
		t.Fatalf("servers = %v, want the bundle entry and the pool entry", servers)
	}
	if servers[0]["serverUrl"] != "https://bundle.example.org" || servers[1]["serverUrl"] != "https://pool.example.org" {
		t.Fatalf("servers = %v, want bundle first, then pool", servers)
	}
}

func TestInjectPoolGiteaServers_PoolWinsOnServerURL(t *testing.T) {
	jcasc := "unclassified:\n  giteaServers:\n    servers:\n      - serverUrl: https://GIT.example.org/\n        manageHooks: true\n"
	entries := []any{map[string]any{"serverUrl": "https://git.example.org", "manageHooks": false}}

	out, err := injectPoolGiteaServers(jcasc, entries)
	if err != nil {
		t.Fatalf("injectPoolGiteaServers: %v", err)
	}
	servers := jcascGiteaServers(t, out)
	if len(servers) != 1 {
		t.Fatalf("servers = %v, want the pool entry to replace the bundle entry", servers)
	}
	if servers[0]["serverUrl"] != "https://git.example.org" || servers[0]["manageHooks"] != false {
		t.Fatalf("servers[0] = %v, want the pool entry", servers[0])
	}
}

func TestInjectPoolGiteaServers_EmptySetRendersEmptyList(t *testing.T) {
	out, err := injectPoolGiteaServers("jenkins:\n  systemMessage: hi\n", []any{})
	if err != nil {
		t.Fatalf("injectPoolGiteaServers: %v", err)
	}
	if !strings.Contains(out, "servers: []") {
		t.Fatalf("jcasc = %q, want an empty servers list", out)
	}
	if got := jcascGiteaServers(t, out); len(got) != 0 {
		t.Fatalf("servers = %v, want none", got)
	}
}

func TestApplyPoolGiteaServers_NonCellNoop(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("plain", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	before := mat.JenkinsYAML

	if err := rec.applyPoolGiteaServers(context.Background(), cr, mat, poolGiteaCoreSet()); err != nil {
		t.Fatalf("applyPoolGiteaServers: %v", err)
	}
	if mat.JenkinsYAML != before {
		t.Fatalf("a non-cell controller must not change the bundle: %q", mat.JenkinsYAML)
	}
}

// TestApplyPoolGiteaServers_SetsAndClearsCondition checks the PoolGiteaServers
// condition across two passes: a rendered server, then a pool that no longer
// declares a server.
func TestApplyPoolGiteaServers_SetsAndClearsCondition(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	seedPoolGiteaPool(client, cr, []v1alpha1.CellPoolGiteaServer{{ServerURL: "https://git.example.org"}})

	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolGiteaServers(context.Background(), cr, mat, poolGiteaCoreSet()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	cond := findControllerCondition(cr, v1alpha1.ConditionPoolGiteaServers)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonPoolGiteaServersRendered {
		t.Fatalf("pass 1 condition = %+v, want True/Rendered", cond)
	}
	if got := jcascGiteaServers(t, mat.JenkinsYAML); len(got) != 1 {
		t.Fatalf("pass 1 servers = %v, want one", got)
	}

	cr.Labels = map[string]string{cells.LabelCellPool: "pool-missing"}
	mat = &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolGiteaServers(context.Background(), cr, mat, poolGiteaCoreSet()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if findControllerCondition(cr, v1alpha1.ConditionPoolGiteaServers) != nil {
		t.Fatal("pass 2 must remove the condition when the pool declares no server")
	}
	if got := jcascGiteaServers(t, mat.JenkinsYAML); len(got) != 0 {
		t.Fatalf("pass 2 servers = %v, want an empty list that unloads the removed server", got)
	}
}

func TestApplyPoolGiteaServers_PluginMissingLeavesJcascUnchanged(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	seedPoolGiteaPool(client, cr, []v1alpha1.CellPoolGiteaServer{{ServerURL: "https://git.example.org"}})

	jcasc := "jenkins:\n  systemMessage: hi\n"
	mat := &bundle.MaterializedBundle{JenkinsYAML: jcasc}
	if err := rec.applyPoolGiteaServers(context.Background(), cr, mat, nil); err != nil {
		t.Fatalf("applyPoolGiteaServers: %v", err)
	}
	if mat.JenkinsYAML != jcasc {
		t.Fatalf("JCasC changed without the gitea plugin; an unknown key would fail the reload:\n%s", mat.JenkinsYAML)
	}
	cond := findControllerCondition(cr, v1alpha1.ConditionPoolGiteaServers)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonPoolGiteaServerPluginMissing {
		t.Fatalf("condition = %+v, want False/PluginMissing", cond)
	}
}

func TestApplyPoolGiteaServers_PoolReadErrorFails(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	client.store.FailAlways("get", gvr, errors.New("apiserver unavailable"))

	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolGiteaServers(context.Background(), cr, mat, poolGiteaCoreSet()); err == nil {
		t.Fatal("a pool read failure must fail the resolve")
	}
}
