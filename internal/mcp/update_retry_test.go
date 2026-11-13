package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// secondWriterAnnotation marks the object a concurrent writer changes during a tool call.
const secondWriterAnnotation = "test.varroa.dev/second-writer"

// raceFirstGet makes a second writer change the object of kind T right after the
// first read of it. The read that the tool makes stays stale, so the tool's
// write must conflict.
func raceFirstGet[T any](t *testing.T, store *crdstore.Fake) {
	t.Helper()
	gvr, err := crdstore.GVRFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	store.SetAfterGet(gvr, func(namespace, name string) {
		if fired {
			return
		}
		fired = true
		ctx := context.Background()
		live, err := store.GetObject(ctx, gvr, namespace, name)
		if err != nil {
			t.Errorf("second writer get: %v", err)
			return
		}
		ann := live.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[secondWriterAnnotation] = "kept"
		live.SetAnnotations(ann)
		if _, err := store.UpdateObject(ctx, gvr, namespace, live); err != nil {
			t.Errorf("second writer update: %v", err)
		}
	})
}

// requireSecondWriterKept fails the test when the concurrent write of kind T is lost.
func requireSecondWriterKept[T any](t *testing.T, store *crdstore.Fake, name, namespace string) {
	t.Helper()
	gvr, err := crdstore.GVRFor[T]()
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.GetObject(context.Background(), gvr, namespace, name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	if live.GetAnnotations()[secondWriterAnnotation] != "kept" {
		t.Errorf("second writer annotation lost on %s: %v", name, live.GetAnnotations())
	}
}

// resultTextOf returns the text of a tool result.
func resultTextOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func jenkinsRoleGVR(t *testing.T) schema.GroupVersionResource {
	t.Helper()
	gvr, err := crdstore.GVRFor[v1alpha1.JenkinsRole]()
	if err != nil {
		t.Fatal(err)
	}
	return gvr
}

func TestUpdateLocal_ConflictOnEveryAttemptAsksForANewCall(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "r1"}})
	gvr := jenkinsRoleGVR(t)
	store.FailAlways("update", gvr, apierrors.NewConflict(gvr.GroupResource(), "r1", errors.New("stale resourceVersion")))
	gets := 0
	store.SetAfterGet(gvr, func(string, string) { gets++ })

	obj, res := updateLocal[v1alpha1.JenkinsRole](context.Background(), store, "r1", "", func(*v1alpha1.JenkinsRole) error { return nil }, updateTexts{
		tool:     "update_jenkins_role",
		object:   "Jenkins role r1",
		notFound: "Jenkins role not found",
		failed:   "failed to update Jenkins role",
	})
	if obj != nil || res == nil {
		t.Fatalf("got object %v and result %v, want a tool error", obj, res)
	}
	want := "Jenkins role r1 changed during each of 3 attempts: call update_jenkins_role again"
	if got := resultTextOf(res); got != want {
		t.Errorf("error text = %q, want %q", got, want)
	}
	if gets != updateWriteAttempts {
		t.Errorf("gets = %d, want %d", gets, updateWriteAttempts)
	}
}

func TestUpdateLocal_StoreConflictIsRetried(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "r1"}})
	gvr := jenkinsRoleGVR(t)
	store.FailNext("update", gvr, apierrors.NewConflict(gvr.GroupResource(), "r1", errors.New("stale resourceVersion")))
	gets := 0
	store.SetAfterGet(gvr, func(string, string) { gets++ })

	obj, res := updateLocal[v1alpha1.JenkinsRole](context.Background(), store, "r1", "", func(obj *v1alpha1.JenkinsRole) error {
		obj.Spec.Description = "new"
		return nil
	}, updateTexts{tool: "update_jenkins_role", object: "Jenkins role r1", notFound: "Jenkins role not found", failed: "failed to update Jenkins role"})
	if res != nil {
		t.Fatalf("unexpected tool error: %v", resultTextOf(res))
	}
	if obj == nil || obj.Spec.Description != "new" {
		t.Errorf("returned object = %+v, want description new", obj)
	}
	if gets != 2 {
		t.Errorf("gets = %d, want 2", gets)
	}
	stored, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), store, "r1", "")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Description != "new" {
		t.Errorf("stored description = %q, want new", stored.Spec.Description)
	}
}

func TestUpdateLocal_MergeErrorWritesNothing(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "r1"}})
	before, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), store, "r1", "")
	if err != nil {
		t.Fatal(err)
	}

	obj, res := updateLocal[v1alpha1.JenkinsRole](context.Background(), store, "r1", "", func(*v1alpha1.JenkinsRole) error {
		return errors.New("invalid x")
	}, updateTexts{tool: "update_jenkins_role", object: "Jenkins role r1", notFound: "Jenkins role not found", failed: "failed to update Jenkins role"})
	if obj != nil || res == nil {
		t.Fatalf("got object %v and result %v, want a tool error", obj, res)
	}
	if got := resultTextOf(res); got != "invalid x" {
		t.Errorf("error text = %q, want %q", got, "invalid x")
	}
	after, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), store, "r1", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion changed from %s to %s", before.ResourceVersion, after.ResourceVersion)
	}
}

func TestUpdateLocal_WriteErrorIsNotRetried(t *testing.T) {
	store := crdstore.NewFake()
	crdstore.MustSeed(store, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "r1"}})
	gvr := jenkinsRoleGVR(t)
	store.FailNext("update", gvr, errors.New("boom"))
	gets := 0
	store.SetAfterGet(gvr, func(string, string) { gets++ })

	_, res := updateLocal[v1alpha1.JenkinsRole](context.Background(), store, "r1", "", func(*v1alpha1.JenkinsRole) error { return nil }, updateTexts{tool: "update_jenkins_role", object: "Jenkins role r1", notFound: "Jenkins role not found", failed: "failed to update Jenkins role"})
	if res == nil {
		t.Fatal("want a tool error")
	}
	if got := resultTextOf(res); !strings.HasPrefix(got, "failed to update Jenkins role:") {
		t.Errorf("error text = %q, want prefix %q", got, "failed to update Jenkins role:")
	}
	if gets != 1 {
		t.Errorf("gets = %d, want 1", gets)
	}
}

func TestUpdateJenkinsRole_ConcurrentWriteSurvives(t *testing.T) {
	deps := adminDeps()
	withLocalOperator(deps)
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.JenkinsRole{ObjectMeta: metav1.ObjectMeta{Name: "r1"}})
	raceFirstGet[v1alpha1.JenkinsRole](t, store)

	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role", map[string]interface{}{"name": "r1", "description": "edited"})
	if tr.IsError {
		t.Fatalf("update_jenkins_role returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.JenkinsRole](context.Background(), store, "r1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Description != "edited" {
		t.Errorf("description = %q, want edited", got.Spec.Description)
	}
	requireSecondWriterKept[v1alpha1.JenkinsRole](t, store, "r1", "")
}

func TestUpdateJenkinsRoleBinding_ConcurrentWriteSurvives(t *testing.T) {
	deps := adminDeps()
	withLocalOperator(deps)
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.JenkinsRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "jrb1"}, Spec: v1alpha1.JenkinsRoleBindingSpec{RoleRef: "old"}})
	raceFirstGet[v1alpha1.JenkinsRoleBinding](t, store)

	tr := callToolAs(t, deps, mcpAdminClaims, "update_jenkins_role_binding", map[string]interface{}{"name": "jrb1", "roleRef": "new"})
	if tr.IsError {
		t.Fatalf("update_jenkins_role_binding returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), store, "jrb1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.RoleRef != "new" {
		t.Errorf("roleRef = %q, want new", got.Spec.RoleRef)
	}
	requireSecondWriterKept[v1alpha1.JenkinsRoleBinding](t, store, "jrb1", "")
}

func TestUpdateVarroaRole_ConcurrentWriteSurvives(t *testing.T) {
	deps := adminDeps()
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.VarroaRole{ObjectMeta: metav1.ObjectMeta{Name: "r1"}, Spec: v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "old"}})
	raceFirstGet[v1alpha1.VarroaRole](t, store)

	tr := callToolAs(t, deps, mcpAdminClaims, "update_varroa_role", map[string]interface{}{"name": "r1", "jenkinsRoleRef": "new"})
	if tr.IsError {
		t.Fatalf("update_varroa_role returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.VarroaRole](context.Background(), store, "r1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.JenkinsRoleRef != "new" {
		t.Errorf("jenkinsRoleRef = %q, want new", got.Spec.JenkinsRoleRef)
	}
	requireSecondWriterKept[v1alpha1.VarroaRole](t, store, "r1", "")
}

func TestUpdateVarroaRoleBinding_ConcurrentWriteSurvives(t *testing.T) {
	deps := adminDeps()
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.VarroaRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "rb1"}, Spec: v1alpha1.VarroaRoleBindingSpec{RoleRef: "old"}})
	raceFirstGet[v1alpha1.VarroaRoleBinding](t, store)

	tr := callToolAs(t, deps, mcpAdminClaims, "update_varroa_role_binding", map[string]interface{}{"name": "rb1", "roleRef": "new"})
	if tr.IsError {
		t.Fatalf("update_varroa_role_binding returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.VarroaRoleBinding](context.Background(), store, "rb1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.RoleRef != "new" {
		t.Errorf("roleRef = %q, want new", got.Spec.RoleRef)
	}
	requireSecondWriterKept[v1alpha1.VarroaRoleBinding](t, store, "rb1", "")
}

func TestUpdateComposedBundle_ConcurrentWriteSurvives(t *testing.T) {
	deps := adminDeps()
	withLocalOperator(deps)
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: "cb1", Namespace: "ns"}, Spec: v1alpha1.ComposedBundleSpec{DisplayName: "old"}})
	raceFirstGet[v1alpha1.ComposedBundle](t, store)

	tr := callToolAs(t, deps, mcpAdminClaims, "update_composed_bundle", map[string]interface{}{"namespace": "ns", "name": "cb1", "displayName": "new"})
	if tr.IsError {
		t.Fatalf("update_composed_bundle returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.ComposedBundle](context.Background(), store, "cb1", "ns")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.DisplayName != "new" {
		t.Errorf("displayName = %q, want new", got.Spec.DisplayName)
	}
	requireSecondWriterKept[v1alpha1.ComposedBundle](t, store, "cb1", "ns")
}

func TestUpdateCatalogSource_ConcurrentWriteSurvives(t *testing.T) {
	deps := adminDeps()
	withLocalOperator(deps)
	store := deps.Store.(*crdstore.Fake)
	crdstore.MustSeed(store, &v1alpha1.CatalogSource{ObjectMeta: metav1.ObjectMeta{Name: "src1", Namespace: "ns"}, Spec: v1alpha1.CatalogSourceSpec{RepoURL: "https://example.com/repo.git", Revision: "v1"}})
	raceFirstGet[v1alpha1.CatalogSource](t, store)

	tr := callToolAs(t, deps, mcpAdminClaims, "update_catalog_source", map[string]interface{}{"namespace": "ns", "name": "src1", "revision": "v2"})
	if tr.IsError {
		t.Fatalf("update_catalog_source returned error: %v", tr.Content)
	}
	got, err := crdstore.Get[v1alpha1.CatalogSource](context.Background(), store, "src1", "ns")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Revision != "v2" {
		t.Errorf("revision = %q, want v2", got.Spec.Revision)
	}
	requireSecondWriterKept[v1alpha1.CatalogSource](t, store, "src1", "ns")
}
