package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// TestBuildPipelineDefinition_SourceNamesWhereTheScriptLives asserts the
// Defined by card's data for the ways an item declares its script: a
// repository, an inline script, a catalog template, and none of them.
func TestBuildPipelineDefinition_SourceNamesWhereTheScriptLives(t *testing.T) {
	scm := &items.Item{Kind: "pipeline", Name: "build", Definition: &items.PipelineDefinition{
		CpsScmFlowDefinition: &items.CpsScmFlowDefinition{SCM: items.SCM{GitSCM: &items.GitSCM{
			UserRemoteConfigs: []items.UserRemoteConfig{{UserRemoteConfig: items.RemoteConfig{URL: "https://git.example.com/payments/api.git"}}},
		}}},
	}}
	got := buildPipelineDefinition(scm)
	if got == nil || got.Source != "jenkinsfile" || got.JenkinsfileRepo != "https://git.example.com/payments/api.git" {
		t.Fatalf("SCM item: got %+v, want source jenkinsfile with the remote URL", got)
	}

	multibranch := &items.Item{Kind: "multibranch", Name: "api", SourcesList: []items.BranchSourceEntry{
		{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{Git: &items.GitSCMSource{Remote: "https://git.example.com/payments/api.git"}}}},
	}}
	got = buildPipelineDefinition(multibranch)
	if got == nil || got.Source != "jenkinsfile" || got.JenkinsfileRepo != "https://git.example.com/payments/api.git" {
		t.Fatalf("multibranch item: got %+v, want source jenkinsfile with the branch source remote", got)
	}

	inline := &items.Item{Kind: "pipeline", Name: "build", Definition: &items.PipelineDefinition{Script: "pipeline { agent any }"}}
	got = buildPipelineDefinition(inline)
	if got == nil || got.Source != "inline" || got.JenkinsfileRepo != "" {
		t.Fatalf("inline item: got %+v, want source inline and no repository", got)
	}

	templated := &items.Item{Kind: "pipeline", Name: "build", Definition: &items.PipelineDefinition{
		VarroaTemplate: &items.VarroaTemplateDefinition{Template: "go-service", TemplateVersion: "2.1.4"},
	}}
	got = buildPipelineDefinition(templated)
	if got == nil || got.Source != "template" || got.JenkinsfileRepo != "" {
		t.Fatalf("template item: got %+v, want source template and no repository", got)
	}

	if got := buildPipelineDefinition(&items.Item{Kind: "pipeline", Name: "bare"}); got != nil {
		t.Fatalf("an item with no definition must produce no row, got %+v", got)
	}
	if got := buildPipelineDefinition(nil); got != nil {
		t.Fatalf("a nil item must produce no row, got %+v", got)
	}
}

// definitionItemContent declares acme/app-a as an inline pipeline. A
// CatalogItem that carries it matches jobPath "acme/app-a".
const definitionItemContent = `
items:
  - kind: folder
    name: acme
    items:
      - kind: pipeline
        name: app-a
        definition:
          script: "pipeline {}"
`

// rootedDefinitionItemContent declares the same job and its own root. A
// composed bundle drops a per-input root at merge time, so a match must
// ignore the key.
const rootedDefinitionItemContent = `
root: legacy
items:
  - kind: folder
    name: acme
    items:
      - kind: pipeline
        name: app-a
        definition:
          script: "pipeline {}"
`

// definitionGitRepo and definitionGitPath name the git input most cases
// resolve.
const (
	definitionGitRepo = "https://github.com/acme/infra"
	definitionGitPath = "bundles/team"
)

// definitionBundleName names every bundle these tests walk.
const definitionBundleName = "bundle-a"

// definitionCatalogNamespace is the namespace every itemRef candidate
// resolves in.
const definitionCatalogNamespace = "team-ns"

// bundleBuilder assembles one ComposedBundle for the declared-input
// tests. Each add call appends one spec input and its status entries by
// index, so spec.inputs, status.inputSummary, and
// status.observedRevisions stay aligned.
type bundleBuilder struct {
	cb *v1alpha1.ComposedBundle
}

// newBundleBuilder starts definitionBundleName in namespace.
func newBundleBuilder(namespace string) *bundleBuilder {
	return &bundleBuilder{cb: &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: definitionBundleName, Namespace: namespace},
		Status:     v1alpha1.ComposedBundleStatus{ObservedRevisions: map[string]string{}},
	}}
}

// addItemRef appends an itemRef input with its resolved summary entry.
func (b *bundleBuilder) addItemRef(name, revision string) *bundleBuilder {
	b.cb.Spec.Inputs = append(b.cb.Spec.Inputs, v1alpha1.ComposedInput{
		ItemRef: &v1alpha1.ComposedItemRef{Name: name},
	})
	b.cb.Status.InputSummary = append(b.cb.Status.InputSummary, v1alpha1.InputSummaryEntry{
		Kind: "itemRef", Type: "item", Namespace: definitionCatalogNamespace,
	})
	return b.setRevision(revision)
}

// addItemRefNoSummary appends an itemRef input and no summary entry, so
// the input's index stays out of range.
func (b *bundleBuilder) addItemRefNoSummary(name string) *bundleBuilder {
	b.cb.Spec.Inputs = append(b.cb.Spec.Inputs, v1alpha1.ComposedInput{
		ItemRef: &v1alpha1.ComposedItemRef{Name: name},
	})
	return b
}

// addItemRefEmptySummary appends an itemRef input whose summary entry is
// present and empty: the input stays unresolved.
func (b *bundleBuilder) addItemRefEmptySummary(name string) *bundleBuilder {
	b.cb.Spec.Inputs = append(b.cb.Spec.Inputs, v1alpha1.ComposedInput{
		ItemRef: &v1alpha1.ComposedItemRef{Name: name},
	})
	b.cb.Status.InputSummary = append(b.cb.Status.InputSummary, v1alpha1.InputSummaryEntry{
		Kind: "itemRef", Type: "item",
	})
	return b
}

// addGitSource appends a gitSource input at revision.
func (b *bundleBuilder) addGitSource(repoURL, path, revision string) *bundleBuilder {
	b.cb.Spec.Inputs = append(b.cb.Spec.Inputs, v1alpha1.ComposedInput{
		GitSource: &v1alpha1.GitBundleSource{RepoURL: repoURL, Path: path},
	})
	b.cb.Status.InputSummary = append(b.cb.Status.InputSummary, v1alpha1.InputSummaryEntry{
		Kind: "gitSource", Type: "git",
	})
	return b.setRevision(revision)
}

// addOCISource appends an ociSource input at revision.
func (b *bundleBuilder) addOCISource(ref, path, revision string) *bundleBuilder {
	b.cb.Spec.Inputs = append(b.cb.Spec.Inputs, v1alpha1.ComposedInput{
		OCISource: &v1alpha1.OCIBundleSource{Ref: ref, Path: path},
	})
	b.cb.Status.InputSummary = append(b.cb.Status.InputSummary, v1alpha1.InputSummaryEntry{
		Kind: "ociSource", Type: "oci",
	})
	return b.setRevision(revision)
}

// setRevision records revision under the index of the newest input. An
// empty revision records no key.
func (b *bundleBuilder) setRevision(revision string) *bundleBuilder {
	if revision != "" {
		b.cb.Status.ObservedRevisions[strconv.Itoa(len(b.cb.Spec.Inputs)-1)] = revision
	}
	return b
}

// seedCatalogItem stores one CatalogItem in client's catalog, creating
// the map on first use. storeFromFake copies it into the CRD store.
func seedCatalogItem(client *fakeResourceClient, name, namespace, content string) {
	if client.catalogItems == nil {
		client.catalogItems = map[string]*v1alpha1.CatalogItem{}
	}
	client.catalogItems[name] = &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.CatalogItemSpec{Type: "item"},
		Status:     v1alpha1.CatalogItemStatus{Content: content},
	}
}

// definitionTestServer wires a server over client's CRDs for the
// declared-input paths. It discards log output: these tests expect the
// warn paths.
func definitionTestServer(client *fakeResourceClient) *Server {
	return NewServer(&Dependencies{
		Store:             storeFromFake(client),
		Client:            client,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		OperatorNamespace: "test-ns",
	})
}

// TestResolveDeclaredInput_ItemRefWinsOverEarlierGitSource asserts an
// itemRef input wins wherever it sits in the input list, so a gitSource
// before it never answers.
func TestResolveDeclaredInput_ItemRefWinsOverEarlierGitSource(t *testing.T) {
	client := newFakeResourceClient()
	seedCatalogItem(client, "items-a", "team-ns", definitionItemContent)
	srv := definitionTestServer(client)

	b := newBundleBuilder("team-ns").
		addGitSource(definitionGitRepo, definitionGitPath, "sha-git").
		addItemRef("items-a", "sha-item")
	got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a")
	if got == nil {
		t.Fatal("a matching itemRef input must resolve, got nil")
	}
	if got.Kind != "itemRef" || got.Name != "items-a" {
		t.Fatalf("kind/name = %q/%q, want itemRef/items-a", got.Kind, got.Name)
	}
	if got.Revision != "sha-item" {
		t.Errorf("revision = %q, want the itemRef input's own revision", got.Revision)
	}
	if got.Bundle != (bundleRefJSON{Namespace: "team-ns", Name: "bundle-a"}) {
		t.Errorf("bundle = %+v, want the declaring bundle", got.Bundle)
	}
	if got.CatalogItem == nil || got.CatalogItem.Name != "items-a" || got.CatalogItem.Namespace != "team-ns" {
		t.Errorf("catalogItem = %+v, want items-a in the resolved namespace team-ns", got.CatalogItem)
	}
	if got.RepoURL != "" || got.OCIRef != "" {
		t.Errorf("repoURL/ociRef = %q/%q, want both empty for an itemRef", got.RepoURL, got.OCIRef)
	}
}

// TestResolveDeclaredInput_NonMatchingItemRefFallsThroughToGitSource
// asserts an itemRef that declares another job leaves the bundle to the
// git/OCI fallback.
func TestResolveDeclaredInput_NonMatchingItemRefFallsThroughToGitSource(t *testing.T) {
	client := newFakeResourceClient()
	seedCatalogItem(client, "items-a", "team-ns", definitionItemContent)
	srv := definitionTestServer(client)

	b := newBundleBuilder("team-ns").
		addItemRef("items-a", "sha-item").
		addGitSource(definitionGitRepo, definitionGitPath, "sha-git")
	got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-b")
	if got == nil {
		t.Fatal("a non-matching itemRef must fall through to the gitSource input, got nil")
	}
	if got.Kind != "gitSource" || got.RepoURL != definitionGitRepo || got.Path != definitionGitPath {
		t.Fatalf("declaredBy = %+v, want the gitSource input", got)
	}
	if got.Revision != "sha-git" {
		t.Errorf("revision = %q, want the gitSource input's own revision", got.Revision)
	}
	if got.Name != definitionGitRepo {
		t.Errorf("name = %q, want the repo URL for a gitSource", got.Name)
	}
}

// TestResolveDeclaredInput_OCISourceOnly asserts a bundle with one OCI
// input answers from it, with the ref as the name and the digest as the
// revision.
func TestResolveDeclaredInput_OCISourceOnly(t *testing.T) {
	srv := definitionTestServer(newFakeResourceClient())
	b := newBundleBuilder("team-ns").
		addOCISource("ghcr.io/acme/bundle:v1", definitionGitPath, "sha256:abc")
	got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a")
	if got == nil {
		t.Fatal("a bundle with one ociSource input must resolve, got nil")
	}
	if got.Kind != "ociSource" || got.OCIRef != "ghcr.io/acme/bundle:v1" || got.Name != "ghcr.io/acme/bundle:v1" {
		t.Fatalf("declaredBy = %+v, want the ociSource input", got)
	}
	if got.Path != definitionGitPath || got.Revision != "sha256:abc" {
		t.Errorf("path/revision = %q/%q, want the input's path and digest", got.Path, got.Revision)
	}
	if got.Bundle != (bundleRefJSON{Namespace: "team-ns", Name: "bundle-a"}) {
		t.Errorf("bundle = %+v, want the declaring bundle", got.Bundle)
	}
}

// TestResolveDeclaredInput_FirstGitSourceWins asserts two gitSource
// inputs resolve in declared order.
func TestResolveDeclaredInput_FirstGitSourceWins(t *testing.T) {
	srv := definitionTestServer(newFakeResourceClient())
	b := newBundleBuilder("team-ns").
		addGitSource("https://github.com/acme/first", "one", "sha-1").
		addGitSource("https://github.com/acme/second", "two", "sha-2")
	got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a")
	if got == nil {
		t.Fatal("a bundle with gitSource inputs must resolve, got nil")
	}
	if got.RepoURL != "https://github.com/acme/first" || got.Revision != "sha-1" {
		t.Fatalf("declaredBy = %+v, want the first gitSource input", got)
	}
}

// TestResolveDeclaredInput_CatalogItemNotFoundContinues asserts a
// deleted CatalogItem leaves the loop on the next candidate.
func TestResolveDeclaredInput_CatalogItemNotFoundContinues(t *testing.T) {
	client := newFakeResourceClient()
	// "items-gone" stays unseeded: the read reports NotFound.
	seedCatalogItem(client, "items-b", "team-ns", definitionItemContent)
	srv := definitionTestServer(client)

	b := newBundleBuilder("team-ns").
		addItemRef("items-gone", "sha-gone").
		addItemRef("items-b", "sha-b")
	got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a")
	if got == nil {
		t.Fatal("a NotFound itemRef must continue to the next candidate, got nil")
	}
	if got.Kind != "itemRef" || got.Name != "items-b" || got.Revision != "sha-b" {
		t.Fatalf("declaredBy = %+v, want the second itemRef candidate", got)
	}
}

// TestResolveDeclaredInput_TransientReadErrorIsTerminal asserts a
// non-NotFound CatalogItem read error reports no provenance at all, even
// when a gitSource input could answer.
func TestResolveDeclaredInput_TransientReadErrorIsTerminal(t *testing.T) {
	client := newFakeResourceClient()
	seedCatalogItem(client, "items-a", "team-ns", definitionItemContent)
	srv := definitionTestServer(client)

	gvr, err := crdstore.GVRFor[v1alpha1.CatalogItem]()
	if err != nil {
		t.Fatalf("GVRFor[CatalogItem]: %v", err)
	}
	client.crdStore.FailNext("get", gvr, errors.New("apiserver unavailable"))

	b := newBundleBuilder("team-ns").
		addItemRef("items-a", "sha-item").
		addGitSource(definitionGitRepo, definitionGitPath, "sha-git")
	if got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a"); got != nil {
		t.Fatalf("declaredBy = %+v, want nil: a transient read error must not let a gitSource answer", got)
	}
}

// TestResolveDeclaredInput_MissingRevisionSentinelLeavesRevisionAbsent
// asserts the operator's sentinel reports no revision.
func TestResolveDeclaredInput_MissingRevisionSentinelLeavesRevisionAbsent(t *testing.T) {
	client := newFakeResourceClient()
	seedCatalogItem(client, "items-a", "team-ns", definitionItemContent)
	srv := definitionTestServer(client)

	b := newBundleBuilder("team-ns").
		addItemRef("items-a", controller.ItemMissingRevision)
	got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a")
	if got == nil || got.Kind != "itemRef" {
		t.Fatalf("declaredBy = %+v, want the resolved itemRef input", got)
	}
	if got.Revision != "" {
		t.Fatalf("revision = %q, want it absent for the missing-revision sentinel", got.Revision)
	}
}

// TestResolveDeclaredInput_UnresolvedInputSummaryIsSkipped asserts an
// itemRef with no resolved namespace is skipped rather than read, and
// that the skip stays inside the loop. The CatalogItem carries no
// namespace, so only a read at namespace "" would find it.
func TestResolveDeclaredInput_UnresolvedInputSummaryIsSkipped(t *testing.T) {
	cases := []struct {
		name  string
		build func(*bundleBuilder) *bundleBuilder
	}{
		{"missing entry", func(b *bundleBuilder) *bundleBuilder {
			return b.addGitSource(definitionGitRepo, definitionGitPath, "sha-git").addItemRefNoSummary("items-a")
		}},
		{"present but empty entry", func(b *bundleBuilder) *bundleBuilder {
			return b.addGitSource(definitionGitRepo, definitionGitPath, "sha-git").addItemRefEmptySummary("items-a")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeResourceClient()
			seedCatalogItem(client, "items-a", "", definitionItemContent)
			srv := definitionTestServer(client)

			b := tc.build(newBundleBuilder("team-ns"))
			got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a")
			if got == nil {
				t.Fatal("an unresolved itemRef must not end the resolution, got nil")
			}
			if got.Kind != "gitSource" {
				t.Fatalf("declaredBy = %+v, want the gitSource fallback: the candidate is skipped, not read", got)
			}
		})
	}
}

// TestResolveDeclaredInput_OnlyUnresolvedItemRefResolvesNothing asserts
// a bundle whose whole input list is one unresolved itemRef reports no
// provenance.
func TestResolveDeclaredInput_OnlyUnresolvedItemRefResolvesNothing(t *testing.T) {
	cases := []struct {
		name  string
		build func(*bundleBuilder) *bundleBuilder
	}{
		{"missing entry", func(b *bundleBuilder) *bundleBuilder { return b.addItemRefNoSummary("items-a") }},
		{"present but empty entry", func(b *bundleBuilder) *bundleBuilder { return b.addItemRefEmptySummary("items-a") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeResourceClient()
			seedCatalogItem(client, "items-a", "team-ns", definitionItemContent)
			srv := definitionTestServer(client)

			b := tc.build(newBundleBuilder("team-ns"))
			if got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a"); got != nil {
				t.Fatalf("declaredBy = %+v, want nil for a bundle with no resolved input", got)
			}
		})
	}
}

// TestResolveDeclaredInput_MalformedCatalogItemContentIsNonMatching
// asserts a CatalogItem whose content does not parse is a non-match, not
// a failure of the call.
func TestResolveDeclaredInput_MalformedCatalogItemContentIsNonMatching(t *testing.T) {
	client := newFakeResourceClient()
	seedCatalogItem(client, "items-a", "team-ns", "items: [oops")
	srv := definitionTestServer(client)

	b := newBundleBuilder("team-ns").
		addItemRef("items-a", "sha-item").
		addGitSource(definitionGitRepo, definitionGitPath, "sha-git")
	got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{}, "acme/app-a")
	if got == nil {
		t.Fatal("a malformed candidate must leave the gitSource fallback intact, got nil")
	}
	if got.Kind != "gitSource" {
		t.Fatalf("declaredBy = %+v, want the gitSource fallback", got)
	}
}

// TestResolveDeclaredInput_DeclaredRootIsCleared asserts a candidate that
// declares its own root still matches, because the merge drops that key
// and the match compares unprefixed paths.
func TestResolveDeclaredInput_DeclaredRootIsCleared(t *testing.T) {
	cases := []struct {
		name    string
		folder  string
		jobPath string
	}{
		{"controller tenant", "", "acme/app-a"},
		{"placed Team", "team-a", "team-a/acme/app-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeResourceClient()
			seedCatalogItem(client, "items-a", "team-ns", rootedDefinitionItemContent)
			srv := definitionTestServer(client)

			b := newBundleBuilder("team-ns").addItemRef("items-a", "sha-item")
			got := srv.resolveDeclaredInput(context.Background(), b.cb, TenantRef{Folder: tc.folder}, tc.jobPath)
			if got == nil {
				t.Fatal("a candidate that declares its own root must still match, got nil")
			}
			if got.Kind != "itemRef" || got.Name != "items-a" {
				t.Fatalf("declaredBy = %+v, want the itemRef candidate", got)
			}
		})
	}
}

// TestResolveDeclaredInput_PlacedTeamFolderReRoot asserts a placed Team
// compares a candidate's items under the Team folder.
func TestResolveDeclaredInput_PlacedTeamFolderReRoot(t *testing.T) {
	client := newFakeResourceClient()
	seedCatalogItem(client, "items-a", "team-ns", definitionItemContent)
	srv := definitionTestServer(client)

	// A placed Team's own bundle lives in the cell's namespace, not in
	// the catalog's.
	b := newBundleBuilder("cells-ns").addItemRef("items-a", "sha-item")
	ref := TenantRef{Folder: "team-a"}
	got := srv.resolveDeclaredInput(context.Background(), b.cb, ref, "team-a/acme/app-a")
	if got == nil {
		t.Fatal("a placed Team's candidate must match in the re-rooted path space, got nil")
	}
	if got.Kind != "itemRef" || got.Name != "items-a" {
		t.Fatalf("declaredBy = %+v, want the itemRef candidate", got)
	}
	if got := srv.resolveDeclaredInput(context.Background(), b.cb, ref, "acme/app-a"); got != nil {
		t.Fatalf("declaredBy = %+v, want nil: a placed Team never matches an unprefixed path", got)
	}
}

// TestRevisionForInput asserts the two-key observedRevisions read and the
// sentinel exclusion.
func TestRevisionForInput(t *testing.T) {
	cases := []struct {
		name        string
		observed    map[string]string
		itemRefName string
		want        string
	}{
		{"index key present", map[string]string{"0": "sha-index", "items-a": "sha-name"}, "items-a", "sha-index"},
		{"index key empty falls back to the itemRef name", map[string]string{"items-a": "sha-name"}, "items-a", "sha-name"},
		{"both keys empty", nil, "items-a", ""},
		{"both keys empty and no itemRef name", nil, "", ""},
		{"sentinel on the index key", map[string]string{"0": controller.ItemMissingRevision}, "items-a", ""},
		{"sentinel on the itemRef name key", map[string]string{"items-a": controller.ItemMissingRevision}, "items-a", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb := &v1alpha1.ComposedBundle{Status: v1alpha1.ComposedBundleStatus{ObservedRevisions: tc.observed}}
			if got := revisionForInput(cb, 0, tc.itemRefName); got != tc.want {
				t.Fatalf("revisionForInput = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLastRunDefinitionPath asserts the decode of a run's source column:
// the stored envelope carries snake_case keys, so the camelCase key
// decodes to nothing.
func TestLastRunDefinitionPath(t *testing.T) {
	cases := []struct {
		name string
		run  *recordstore.Run
		want string
	}{
		{"nil last run", nil, ""},
		{"empty source", &recordstore.Run{}, ""},
		{"snake_case definition_path", &recordstore.Run{Source: json.RawMessage(`{"definition_path":"Jenkinsfile"}`)}, "Jenkinsfile"},
		{"camelCase definitionPath", &recordstore.Run{Source: json.RawMessage(`{"definitionPath":"Jenkinsfile"}`)}, ""},
		{"malformed JSON", &recordstore.Run{Source: json.RawMessage(`{"definition_path":`)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastRunDefinitionPath(tc.run); got != tc.want {
				t.Fatalf("lastRunDefinitionPath = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFindDeclaredItem_BundleRef asserts declaredBy.bundle names the
// composed bundle the tenant reads.
func TestFindDeclaredItem_BundleRef(t *testing.T) {
	t.Run("a controller with no ref reads the starter bundle", func(t *testing.T) {
		client := newFakeResourceClient()
		client.controllers = map[string]*v1alpha1.Controller{
			"ctrl-a": {ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"}},
		}
		client.composedBundles = map[string]*v1alpha1.ComposedBundle{
			v1alpha1.StarterBundleName: {
				ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.StarterBundleName, Namespace: "test-ns"},
				Spec: v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{
					{GitSource: &v1alpha1.GitBundleSource{RepoURL: definitionGitRepo, Path: definitionGitPath}},
				}},
				Status: v1alpha1.ComposedBundleStatus{
					ContentRef:        "starter-content",
					ObservedRevisions: map[string]string{"0": "sha-git"},
				},
			},
		}
		client.configMaps = map[string]map[string]string{
			"starter-content": {"items.yaml": singlePipelineItemsYAML},
		}
		srv := definitionTestServer(client)

		_, item, declaredBy, err := srv.findDeclaredItem(context.Background(),
			TenantRef{Namespace: "test-ns", Controller: "ctrl-a"}, "acme/app-a")
		if err != nil {
			t.Fatalf("findDeclaredItem: %v", err)
		}
		if item == nil {
			t.Fatal("acme/app-a must resolve from the starter bundle")
		}
		if declaredBy == nil {
			t.Fatal("a job with a definition must carry provenance")
		}
		if want := (bundleRefJSON{Namespace: "test-ns", Name: v1alpha1.StarterBundleName}); declaredBy.Bundle != want {
			t.Fatalf("bundle = %+v, want the operator namespace's starter bundle %+v", declaredBy.Bundle, want)
		}
		if declaredBy.Kind != "gitSource" || declaredBy.Revision != "sha-git" {
			t.Errorf("kind/revision = %q/%q, want the starter bundle's git input", declaredBy.Kind, declaredBy.Revision)
		}
	})

	t.Run("a controller with an explicit ref reads that ref", func(t *testing.T) {
		client := newFakeResourceClient()
		client.controllers = map[string]*v1alpha1.Controller{
			"ctrl-a": {
				ObjectMeta: metav1.ObjectMeta{Name: "ctrl-a", Namespace: "test-ns"},
				Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-b", Namespace: "team-ns"}},
			},
		}
		client.composedBundles = map[string]*v1alpha1.ComposedBundle{
			"bundle-b": {
				ObjectMeta: metav1.ObjectMeta{Name: "bundle-b", Namespace: "team-ns"},
				Spec: v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{
					{GitSource: &v1alpha1.GitBundleSource{RepoURL: definitionGitRepo, Path: definitionGitPath}},
				}},
				Status: v1alpha1.ComposedBundleStatus{ContentRef: "bundle-b-content"},
			},
		}
		client.configMaps = map[string]map[string]string{
			"bundle-b-content": {"items.yaml": singlePipelineItemsYAML},
		}
		srv := definitionTestServer(client)

		_, item, declaredBy, err := srv.findDeclaredItem(context.Background(),
			TenantRef{Namespace: "test-ns", Controller: "ctrl-a"}, "acme/app-a")
		if err != nil {
			t.Fatalf("findDeclaredItem: %v", err)
		}
		if item == nil || declaredBy == nil {
			t.Fatalf("item/declaredBy = %v/%v, want both present", item, declaredBy)
		}
		if want := (bundleRefJSON{Namespace: "team-ns", Name: "bundle-b"}); declaredBy.Bundle != want {
			t.Fatalf("bundle = %+v, want the controller's explicit ref %+v", declaredBy.Bundle, want)
		}
	})

	t.Run("a placed Team reads its own bundle, not the cell's", func(t *testing.T) {
		client := newFakeResourceClient()
		client.controllers = map[string]*v1alpha1.Controller{
			"ctrl-b": {
				ObjectMeta: metav1.ObjectMeta{Name: "ctrl-b", Namespace: "test-ns"},
				Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-cell"}},
			},
		}
		client.composedBundles = map[string]*v1alpha1.ComposedBundle{
			"bundle-cell": {
				ObjectMeta: metav1.ObjectMeta{Name: "bundle-cell", Namespace: "test-ns"},
				Spec: v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{
					{GitSource: &v1alpha1.GitBundleSource{RepoURL: "https://github.com/acme/cell", Path: "cell"}},
				}},
				Status: v1alpha1.ComposedBundleStatus{ContentRef: "bundle-cell-content"},
			},
			"bundle-team": {
				ObjectMeta: metav1.ObjectMeta{Name: "bundle-team", Namespace: "team-ns"},
				Spec: v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{
					{GitSource: &v1alpha1.GitBundleSource{RepoURL: definitionGitRepo, Path: definitionGitPath}},
				}},
				Status: v1alpha1.ComposedBundleStatus{
					ContentRef:        "bundle-team-content",
					ObservedRevisions: map[string]string{"0": "sha-tenant"},
				},
			},
		}
		client.configMaps = map[string]map[string]string{
			"bundle-cell-content": {"items.yaml": singlePipelineItemsYAML},
			"bundle-team-content": {"items.yaml": singlePipelineItemsYAML},
		}
		srv := definitionTestServer(client)
		seedTeam(t, client.crdStore, placedTeamObjectWithBundle("team-a", "cluster-1", "cells-ns", "ctrl-b", "bundle-team", "team-ns"))

		_, item, declaredBy, err := srv.findDeclaredItem(context.Background(),
			TenantRef{Namespace: "test-ns", Controller: "ctrl-b", Team: "team-a", Folder: "team-a"}, "team-a/acme/app-a")
		if err != nil {
			t.Fatalf("findDeclaredItem: %v", err)
		}
		if item == nil || declaredBy == nil {
			t.Fatalf("item/declaredBy = %v/%v, want both present", item, declaredBy)
		}
		if want := (bundleRefJSON{Namespace: "team-ns", Name: "bundle-team"}); declaredBy.Bundle != want {
			t.Fatalf("bundle = %+v, want the Team's own bundle %+v", declaredBy.Bundle, want)
		}
		if declaredBy.Revision != "sha-tenant" {
			t.Errorf("revision = %q, want the Team bundle input's own revision", declaredBy.Revision)
		}
	})
}

func TestBuildPipelineTriggersGitHubPushShowsPush(t *testing.T) {
	item := &items.Item{Kind: "pipeline", Name: "app", Triggers: []items.Trigger{
		{GitHubPush: &items.GitHubPushTrigger{}},
	}}
	got := buildPipelineTriggers(item)
	if got == nil || !got.Push {
		t.Fatalf("buildPipelineTriggers() = %+v, want Push true", got)
	}
	if got.Schedule != "" {
		t.Errorf("Schedule = %q, want empty", got.Schedule)
	}
}

func TestBuildPipelineTriggersGenericTriggerShowsWebhook(t *testing.T) {
	item := &items.Item{Kind: "pipeline", Name: "app", Triggers: []items.Trigger{
		{GenericTrigger: &items.GenericTrigger{Token: "payments:deploy"}},
	}}
	got := buildPipelineTriggers(item)
	if got == nil || !got.Webhook {
		t.Fatalf("buildPipelineTriggers() = %+v, want Webhook true", got)
	}
	if got.Push || got.Schedule != "" {
		t.Errorf("Push = %v, Schedule = %q, want false and empty", got.Push, got.Schedule)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if s := string(raw); strings.Contains(s, "payments:deploy") || strings.Contains(s, "token") {
		t.Errorf("triggers JSON leaks the webhook token: %s", s)
	}
}

func TestBuildPipelineTriggersGenericTriggerInPipelineTriggersProperty(t *testing.T) {
	item := &items.Item{Kind: "pipeline", Name: "app", Properties: []items.Property{
		{PipelineTriggers: &items.PipelineTriggers{Triggers: []items.Trigger{
			{GenericTrigger: &items.GenericTrigger{Token: "payments:deploy"}},
		}}},
	}}
	got := buildPipelineTriggers(item)
	if got == nil || !got.Webhook {
		t.Fatalf("buildPipelineTriggers() = %+v, want Webhook true", got)
	}
}

func TestBuildPipelineTriggersPollSCMDefaultSchedule(t *testing.T) {
	for _, spec := range []string{"", "  "} {
		item := &items.Item{Kind: "pipeline", Name: "app", Triggers: []items.Trigger{
			{PollSCM: &items.PollSCMTrigger{ScmpollSpec: spec}},
		}}
		got := buildPipelineTriggers(item)
		if got == nil || got.Schedule != "H * * * *" {
			t.Errorf("spec %q: buildPipelineTriggers() = %+v, want Schedule %q", spec, got, "H * * * *")
		}
	}
}

func TestMultibranchGiteaRemoteAndDiscoveryTraits(t *testing.T) {
	sources := []items.BranchSourceEntry{
		{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{Gitea: &items.GiteaSCMSource{
			ServerURL:  "https://git.example.org/",
			RepoOwner:  "acme",
			Repository: "app",
		}}}},
	}
	if got, want := multibranchRemoteURL(sources), "https://git.example.org/acme/app"; got != want {
		t.Fatalf("gitea remote URL = %q, want %q", got, want)
	}
	push, pr := multibranchDiscoveryTraits(sources)
	if !push || !pr {
		t.Fatalf("gitea defaults: push=%v pullRequest=%v, want both true", push, pr)
	}

	onlyFork := []items.BranchSourceEntry{
		{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{Gitea: &items.GiteaSCMSource{
			ServerURL:  "https://git.example.org",
			RepoOwner:  "acme",
			Repository: "app",
			Traits: []items.GiteaTrait{
				{GiteaForkDiscovery: &items.GiteaForkTrait{StrategyID: 1, Trust: "everyone"}},
			},
		}}}},
	}
	push, pr = multibranchDiscoveryTraits(onlyFork)
	if push || !pr {
		t.Fatalf("fork-only gitea traits: push=%v pullRequest=%v, want push false and pullRequest true", push, pr)
	}
}

func TestMultibranchGitHubForkDiscoveryCountsAsPullRequest(t *testing.T) {
	onlyFork := []items.BranchSourceEntry{
		{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{GitHub: &items.GitHubSCMSource{
			RepoOwner:  "acme",
			Repository: "app",
			Traits: []items.GitHubTrait{
				{GitHubForkDiscovery: &items.GitHubForkDiscovery{StrategyID: 1}},
			},
		}}}},
	}
	push, pr := multibranchDiscoveryTraits(onlyFork)
	if push || !pr {
		t.Fatalf("fork-only github traits: push=%v pullRequest=%v, want push false and pullRequest true", push, pr)
	}
}

// tenantUnknownKeyItems holds one item with a key the strict parser refuses.
const tenantUnknownKeyItems = `items:
  - kind: freeStyle
    name: job
    notAKey: x
`

// tenantVarInKeyItems names its builder field with a variable. Resolving
// the variable first gives a valid manifest.
const tenantVarInKeyItems = `items:
  - kind: freeStyle
    name: job
    ${field}:
      - shell:
          command: echo hi
`

// TestControllerItemManifest_PlacedTeamUnknownKeyGivesNoManifest asserts a
// placed Team whose items.yaml holds an unknown key gives no definition, as
// the cell holds that tenant.
func TestControllerItemManifest_PlacedTeamUnknownKeyGivesNoManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantUnknownKeyItems}
	srv := definitionTestServer(client)

	ref := TenantRef{Folder: "payments", Team: "payments", Controller: "cell-1", Namespace: "test-ns"}
	_, _, manifest, err := srv.controllerItemManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the cell holds a tenant with an unknown key", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamVarInKeyKeepsManifest asserts a
// placed Team that names a key with ${var} keeps its definition: the
// variable resolves before the strict parse, as in the cell.
func TestControllerItemManifest_PlacedTeamVarInKeyKeepsManifest(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{
		"items.yaml":     tenantVarInKeyItems,
		"variables.yaml": "field: builders\n",
	}
	srv := definitionTestServer(client)

	ref := TenantRef{Folder: "payments", Team: "payments", Controller: "cell-1", Namespace: "test-ns"}
	_, _, manifest, err := srv.controllerItemManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil || len(manifest.Items) != 1 {
		t.Fatalf("manifest = %+v, want one item after variable resolution", manifest)
	}
}

// TestControllerItemManifest_PlacedTeamWithNoLocalControllerParsesStrictly
// asserts the strict read does not depend on a controller in the Team's
// own namespace. The gate is the placement, not the controller.
func TestControllerItemManifest_PlacedTeamWithNoLocalControllerParsesStrictly(t *testing.T) {
	client := newPlacedTeamFixture(t)
	client.configMaps["payments-content"] = map[string]string{"items.yaml": tenantUnknownKeyItems}
	srv := definitionTestServer(client)

	ref := TenantRef{Folder: "payments", Team: "payments", Controller: "absent", Namespace: "test-ns"}
	_, _, manifest, err := srv.controllerItemManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: a placed Team with no local controller parses strictly", manifest)
	}
}

// TestControllerItemManifest_SyntheticTenantStaysLenient asserts a
// single-controller tenant (no folder) keeps the lenient parse, so an
// unknown key does not hide its definition.
func TestControllerItemManifest_SyntheticTenantStaysLenient(t *testing.T) {
	client := newMultibranchControllerFixture()
	client.configMaps["bundle-a-content"] = map[string]string{"items.yaml": tenantUnknownKeyItems}
	srv := definitionTestServer(client)

	ref := TenantRef{Controller: "ctrl-a", Namespace: "test-ns"}
	_, _, manifest, err := srv.controllerItemManifest(context.Background(), ref)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil || len(manifest.Items) != 1 {
		t.Fatalf("manifest = %+v, want one item from the lenient parse", manifest)
	}
}

// TestMultibranchRemote_NamesForgeOfSource asserts each source form names
// its forge, and that the URL matches multibranchRemoteURL.
func TestMultibranchRemote_NamesForgeOfSource(t *testing.T) {
	cases := []struct {
		name    string
		sources []items.BranchSourceEntry
		forge   string
	}{
		{"gitea", []items.BranchSourceEntry{{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{Gitea: &items.GiteaSCMSource{
			ServerURL: "https://gitea.example.com", RepoOwner: "acme", Repository: "svc"}}}}}, "gitea"},
		{"branchSource github", []items.BranchSourceEntry{{BranchSource: items.BranchSource{Source: items.BranchSourceConfig{GitHub: &items.GitHubSCMSource{
			RepositoryURL: "https://github.com/acme/svc"}}}}}, "github"},
		{"legacy github", []items.BranchSourceEntry{{GitHub: &items.GitHubSCMSource{RepositoryURL: "https://github.com/acme/svc"}}}, "github"},
		{"git", []items.BranchSourceEntry{{Git: &items.GitSCMSource{Remote: "https://git.example.org/acme/svc"}}}, ""},
	}
	for _, tc := range cases {
		url, forge := multibranchRemote(tc.sources)
		if forge != tc.forge {
			t.Errorf("%s: forge = %q, want %q", tc.name, forge, tc.forge)
		}
		if url != multibranchRemoteURL(tc.sources) {
			t.Errorf("%s: url = %q, differs from multibranchRemoteURL", tc.name, url)
		}
	}
}
