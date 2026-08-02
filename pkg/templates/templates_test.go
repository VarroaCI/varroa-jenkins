package templates

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestValidateValid(t *testing.T) {
	tmpl := &v1alpha1.PodTemplate{
		Spec: v1alpha1.PodTemplateSpec{
			Containers: []v1alpha1.ContainerSpec{
				{Name: "jnlp", Image: "jenkins/inbound-agent"},
			},
			RestartPolicy: "Never",
		},
	}
	if err := Validate(tmpl); err != nil {
		t.Errorf("expected valid, got: %v", err)
	}
}

func TestValidateNoContainers(t *testing.T) {
	tmpl := &v1alpha1.PodTemplate{}
	err := Validate(tmpl)
	if err == nil {
		t.Error("expected error for no containers")
	}
}

func TestValidateMissingName(t *testing.T) {
	tmpl := &v1alpha1.PodTemplate{
		Spec: v1alpha1.PodTemplateSpec{
			Containers: []v1alpha1.ContainerSpec{
				{Image: "some-image"},
			},
		},
	}
	err := Validate(tmpl)
	if err == nil {
		t.Error("expected error for missing container name")
	}
}

func TestValidateMissingImage(t *testing.T) {
	tmpl := &v1alpha1.PodTemplate{
		Spec: v1alpha1.PodTemplateSpec{
			Containers: []v1alpha1.ContainerSpec{
				{Name: "mycontainer"},
			},
		},
	}
	err := Validate(tmpl)
	if err == nil {
		t.Error("expected error for missing container image")
	}
}

func TestValidateBadRestartPolicy(t *testing.T) {
	tmpl := &v1alpha1.PodTemplate{
		Spec: v1alpha1.PodTemplateSpec{
			Containers: []v1alpha1.ContainerSpec{
				{Name: "c", Image: "img"},
			},
			RestartPolicy: "InvalidPolicy",
		},
	}
	err := Validate(tmpl)
	if err == nil {
		t.Error("expected error for invalid restart policy")
	}
}

func TestRestartPolicyEnum(t *testing.T) {
	if !RestartPolicyEnum["Always"] {
		t.Error("Always should be valid")
	}
	if !RestartPolicyEnum["OnFailure"] {
		t.Error("OnFailure should be valid")
	}
	if !RestartPolicyEnum["Never"] {
		t.Error("Never should be valid")
	}
	if RestartPolicyEnum["invalid"] {
		t.Error("invalid should not be valid")
	}
}

func TestCatalogRegister(t *testing.T) {
	c := NewCatalog()
	tmpl := &v1alpha1.PodTemplate{
		Spec: v1alpha1.PodTemplateSpec{
			Containers: []v1alpha1.ContainerSpec{
				{Name: "maven", Image: "maven:3.9"},
			},
			RestartPolicy: "Never",
		},
	}
	if err := c.Register("default", tmpl); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestCatalogGet(t *testing.T) {
	c := NewCatalog()
	tmpl := &v1alpha1.PodTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "std"},
		Spec:       v1alpha1.PodTemplateSpec{Containers: []v1alpha1.ContainerSpec{{Name: "x", Image: "x"}}},
	}
	c.Register("ns1", tmpl)

	got, ok := c.Get("ns1", "std")
	if !ok {
		t.Fatal("expected to find template")
	}
	if got.Name != "std" {
		t.Errorf("unexpected name: %s", got.Name)
	}
}

func TestCatalogGetNotFound(t *testing.T) {
	c := NewCatalog()
	_, ok := c.Get("ns1", "missing")
	if ok {
		t.Error("expected not found")
	}
}

func TestCatalogList(t *testing.T) {
	c := NewCatalog()
	c.Register("ns1", &v1alpha1.PodTemplate{Spec: v1alpha1.PodTemplateSpec{Containers: []v1alpha1.ContainerSpec{{Name: "x", Image: "x"}}}})
	c.Register("ns2", &v1alpha1.PodTemplate{Spec: v1alpha1.PodTemplateSpec{Containers: []v1alpha1.ContainerSpec{{Name: "y", Image: "y"}}}})

	all := c.List("")
	if len(all) != 2 {
		t.Errorf("expected 2, got %d", len(all))
	}
	ns1 := c.List("ns1")
	if len(ns1) != 1 {
		t.Errorf("expected 1 in ns1, got %d", len(ns1))
	}
}

func TestCatalogDelete(t *testing.T) {
	c := NewCatalog()
	c.Register("ns1", &v1alpha1.PodTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       v1alpha1.PodTemplateSpec{Containers: []v1alpha1.ContainerSpec{{Name: "x", Image: "x"}}},
	})
	if !c.Delete("ns1", "a") {
		t.Error("expected successful delete")
	}
	_, ok := c.Get("ns1", "a")
	if ok {
		t.Error("expected template to be deleted")
	}
}

func TestCatalogDeleteNotFound(t *testing.T) {
	c := NewCatalog()
	if c.Delete("ns1", "missing") {
		t.Error("expected false for missing template")
	}
}

func TestValidateRegisterFails(t *testing.T) {
	c := NewCatalog()
	bad := &v1alpha1.PodTemplate{}
	err := c.Register("ns1", bad)
	if err == nil {
		t.Error("expected validation error")
	}
}
