package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func newControllerListTestClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(
		&v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "alpha", Namespace: "team-a"}},
		&v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "beta", Namespace: "team-b"}},
	).Build()
}

func TestCacheControllerListSource_NamespaceFilter(t *testing.T) {
	src := NewCacheControllerListSource(newControllerListTestClient(t))

	all, err := src.ListControllers(context.Background(), "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all namespaces: got %d rows, want 2", len(all))
	}

	teamA, err := src.ListControllers(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("list team-a: %v", err)
	}
	if len(teamA) != 1 || teamA[0].Name != "alpha" {
		t.Fatalf("team-a rows = %+v, want only alpha", teamA)
	}
}

func TestCacheControllerListSource_SetsTypeMeta(t *testing.T) {
	rows, err := NewCacheControllerListSource(newControllerListTestClient(t)).ListControllers(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, cr := range rows {
		if cr.APIVersion != "varroa.dev/v1alpha1" || cr.Kind != "Controller" {
			t.Errorf("%s: apiVersion=%q kind=%q", cr.Name, cr.APIVersion, cr.Kind)
		}
	}
}

func TestCacheControllerListSource_ResultsAreCopies(t *testing.T) {
	src := NewCacheControllerListSource(newControllerListTestClient(t))

	first, err := src.ListControllers(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("first list: %v", err)
	}
	first[0].Name = "mutated"
	first[0].Labels = map[string]string{"x": "y"}

	second, err := src.ListControllers(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("second list: %v", err)
	}
	if second[0].Name != "alpha" || second[0].Labels != nil {
		t.Fatalf("second result changed by mutating the first: %+v", second[0].ObjectMeta)
	}
}
