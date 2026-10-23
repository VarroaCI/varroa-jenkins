package items

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/internal/jenkins"
)

func TestReRoot(t *testing.T) {
	in := &Manifest{
		Items: []Item{
			{Kind: "pipeline", Name: "build", Definition: &PipelineDefinition{Script: "echo hi"}},
		},
	}

	out, err := ReRoot(in, "team-a")
	if err != nil {
		t.Fatalf("ReRoot: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("expected exactly one top-level item, got %d", len(out.Items))
	}
	top := out.Items[0]
	if top.Kind != "folder" || top.Name != "team-a" {
		t.Fatalf("expected a folder named team-a, got kind=%q name=%q", top.Kind, top.Name)
	}
	if len(top.Items) != 1 || top.Items[0].Name != "build" {
		t.Fatalf("expected the input item as a child of the tenant folder, got %+v", top.Items)
	}
}

func TestReRoot_RejectsGenericTokenOutsideTenantPrefix(t *testing.T) {
	gen := func(token string) Trigger {
		return Trigger{GenericTrigger: &GenericTrigger{Token: token}}
	}
	tests := []struct {
		name    string
		item    Item
		wantErr bool
	}{
		{"own prefix", Item{Kind: "freeStyle", Name: "a", Triggers: []Trigger{gen("billing:deploy")}}, false},
		{"other prefix", Item{Kind: "freeStyle", Name: "a", Triggers: []Trigger{gen("payments:deploy")}}, true},
		{"bare prefix with no name", Item{Kind: "freeStyle", Name: "a", Triggers: []Trigger{gen("billing:")}}, true},
		{"nested other prefix", Item{Kind: "folder", Name: "f", Items: []Item{
			{Kind: "freeStyle", Name: "a", Triggers: []Trigger{gen("payments:deploy")}},
		}}, true},
		{"pipelineTriggers other prefix", Item{Kind: "pipeline", Name: "a", Properties: []Property{
			{PipelineTriggers: &PipelineTriggers{Triggers: []Trigger{gen("payments:deploy")}}},
		}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReRoot(&Manifest{Items: []Item{tt.item}}, "billing")
			if (err != nil) != tt.wantErr {
				t.Fatalf("ReRoot error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckGenericTokensErrorOmitsToken(t *testing.T) {
	const secret = "secret-value"
	items := []Item{{
		Kind:     "freeStyle",
		Name:     "deploy-job",
		Triggers: []Trigger{{GenericTrigger: &GenericTrigger{Token: "other:" + secret}}},
	}}

	err := checkGenericTokens(items, "payments")
	if err == nil {
		t.Fatal("expected an error for a token outside the tenant prefix")
	}
	msg := err.Error()
	for _, want := range []string{"payments", "deploy-job", "payments:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, secret) {
		t.Errorf("error %q leaks the genericTrigger token", msg)
	}
}

func TestReRoot_RejectsNonEmptyRoot(t *testing.T) {
	in := &Manifest{Root: "some/prefix", Items: []Item{{Kind: "folder", Name: "x"}}}

	if _, err := ReRoot(in, "team-a"); err == nil {
		t.Fatal("expected an error for a manifest with a non-empty root")
	}
}

func TestMerge_TwoTenants(t *testing.T) {
	a, err := ReRoot(&Manifest{Items: []Item{{Kind: "folder", Name: "job-a"}}}, "team-a")
	if err != nil {
		t.Fatalf("ReRoot team-a: %v", err)
	}
	b, err := ReRoot(&Manifest{Items: []Item{{Kind: "folder", Name: "job-b"}}}, "team-b")
	if err != nil {
		t.Fatalf("ReRoot team-b: %v", err)
	}

	merged, err := Merge([]*Manifest{a, b})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Items) != 2 {
		t.Fatalf("expected two top-level items, got %d", len(merged.Items))
	}
	names := map[string]bool{}
	for _, it := range merged.Items {
		names[it.Name] = true
	}
	if !names["team-a"] || !names["team-b"] {
		t.Fatalf("expected team-a and team-b, got %+v", merged.Items)
	}
	if merged.RemoveStrategy == nil || merged.RemoveStrategy.Items != RemoveSync {
		t.Fatalf("expected the merged manifest's remove strategy to be sync, got %+v", merged.RemoveStrategy)
	}
}

func TestMerge_RejectsDuplicateTopLevelName(t *testing.T) {
	a, _ := ReRoot(&Manifest{}, "team-a")
	b, _ := ReRoot(&Manifest{}, "team-a")

	if _, err := Merge([]*Manifest{a, b}); err == nil {
		t.Fatal("expected an error for a duplicate top-level item name")
	}
}

// TestMerge_DroppingATenantDeletesItsFolder proves that Merge's fixed sync
// remove strategy, applied through the engine, deletes a tenant's folder
// once that tenant is no longer part of the merged manifest.
func TestMerge_DroppingATenantDeletesItsFolder(t *testing.T) {
	ctx := context.Background()

	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/doDelete"):
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	client := jenkins.NewClient(srv.URL, "test", "token")
	mf := filepath.Join(t.TempDir(), "managed.json")
	engine := &Engine{client: client, managedFile: mf}

	tenantA, err := ReRoot(&Manifest{}, "team-a")
	if err != nil {
		t.Fatalf("ReRoot team-a: %v", err)
	}
	tenantB, err := ReRoot(&Manifest{}, "team-b")
	if err != nil {
		t.Fatalf("ReRoot team-b: %v", err)
	}

	firstMerge, err := Merge([]*Manifest{tenantA, tenantB})
	if err != nil {
		t.Fatalf("Merge (both tenants): %v", err)
	}
	firstYAML, err := yaml.Marshal(firstMerge)
	if err != nil {
		t.Fatalf("marshal first merge: %v", err)
	}
	if _, err := engine.Apply(ctx, string(firstYAML)); err != nil {
		t.Fatalf("apply (both tenants): %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("expected no deletions on the first apply, got %v", deleted)
	}

	secondMerge, err := Merge([]*Manifest{tenantA})
	if err != nil {
		t.Fatalf("Merge (team-a only): %v", err)
	}
	secondYAML, err := yaml.Marshal(secondMerge)
	if err != nil {
		t.Fatalf("marshal second merge: %v", err)
	}
	if _, err := os.Stat(mf); err != nil {
		t.Fatalf("expected the managed cache to persist between applies: %v", err)
	}
	if _, err := engine.Apply(ctx, string(secondYAML)); err != nil {
		t.Fatalf("apply (team-a only): %v", err)
	}

	found := false
	for _, d := range deleted {
		if strings.Contains(d, "team-b") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected team-b's folder to be deleted once dropped from the merged manifest, deleted=%v", deleted)
	}
}

func TestReRoot_DropsRetain(t *testing.T) {
	in, err := Parse("retain: [other]\nitems:\n  - kind: folder\n    name: f\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := ReRoot(in, "payments")
	if err != nil {
		t.Fatalf("reroot: %v", err)
	}
	if len(out.Retain) != 0 {
		t.Errorf("a tenant manifest must not carry a retain list, got %v", out.Retain)
	}
}
