package controller

import (
	"context"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// pathModeController returns a Provisioning-phase CR configured for path routing.
func pathModeController(host string) *v1alpha1.Controller {
	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: host, Mode: v1alpha1.RoutingModePath, TLSSecretName: "leftover"}
	return cr
}

func TestProvisioningIngressPathMode(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := pathModeController("varroa.example.com")
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(client.ingressCalls) == 0 {
		t.Fatal("Ingress should be created")
	}
	call := client.ingressCalls[0]
	if call.pathPrefix != "/jenkins/ns1/test" {
		t.Errorf("pathPrefix = %q, want /jenkins/ns1/test", call.pathPrefix)
	}
	if call.tlsSecret != "" {
		t.Errorf("tlsSecret = %q, want empty (shared-host TLS is chart-owned in path mode)", call.tlsSecret)
	}
	if call.host != "varroa.example.com" {
		t.Errorf("host = %q, want varroa.example.com", call.host)
	}

	if len(client.stsSpecs) == 0 {
		t.Fatal("StatefulSet should be created")
	}
	if got := client.stsSpecs[0].PathPrefix; got != "/jenkins/ns1/test" {
		t.Errorf("StatefulSetSpec.PathPrefix = %q, want /jenkins/ns1/test", got)
	}
}

func TestProvisioningIngressSubdomainMode(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: "ci.example.com", TLSSecretName: "ci-tls"}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(client.ingressCalls) == 0 {
		t.Fatal("Ingress should be created")
	}
	call := client.ingressCalls[0]
	if call.pathPrefix != "" {
		t.Errorf("pathPrefix = %q, want empty in subdomain mode", call.pathPrefix)
	}
	if call.tlsSecret != "ci-tls" {
		t.Errorf("tlsSecret = %q, want ci-tls passed through", call.tlsSecret)
	}

	if len(client.stsSpecs) == 0 {
		t.Fatal("StatefulSet should be created")
	}
	if got := client.stsSpecs[0].PathPrefix; got != "" {
		t.Errorf("StatefulSetSpec.PathPrefix = %q, want empty", got)
	}
}

// TestRoutingVarsDerivation covers the three rows of the mode-aware var table:
// no/empty host, subdomain mode, and path mode.
func TestRoutingVarsDerivation(t *testing.T) {
	const baseEndpoint = "http://test-00000000-svc.ns1.svc.cluster.local:8080"

	tests := []struct {
		name        string
		ingressSpec *v1alpha1.IngressSpec
		wantExtURL  string
		wantPrefix  string
		wantEp      string
	}{
		{
			name:        "no ingress spec",
			ingressSpec: nil,
			wantExtURL:  "",
			wantPrefix:  "",
			wantEp:      baseEndpoint,
		},
		{
			name:        "subdomain mode",
			ingressSpec: &v1alpha1.IngressSpec{Host: "ci.example.com"},
			wantExtURL:  "https://ci.example.com",
			wantPrefix:  "",
			wantEp:      baseEndpoint,
		},
		{
			name:        "path mode",
			ingressSpec: &v1alpha1.IngressSpec{Host: "varroa.example.com", Mode: v1alpha1.RoutingModePath},
			wantExtURL:  "https://varroa.example.com/jenkins/ns1/test",
			wantPrefix:  "/jenkins/ns1/test",
			wantEp:      baseEndpoint + "/jenkins/ns1/test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := newTestClientWithBundle()
			client.configMapData["test-bundle-content"]["jenkins.yaml"] = "ext: \"${varroa_controller_external_url}\"\nprefix: \"${varroa_controller_path_prefix}\"\nep: \"${varroa_controller_endpoint}\"\n"
			rec := newTestReconciler(client)

			cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
			cr.Spec.IngressSpec = tt.ingressSpec

			mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
			if err != nil {
				t.Fatalf("resolveBundleForController: %v", err)
			}

			got := map[string]interface{}{}
			if err := yaml.Unmarshal([]byte(mat.JenkinsYAML), &got); err != nil {
				t.Fatalf("parse resolved jenkins.yaml: %v", err)
			}
			if got["ext"] != tt.wantExtURL {
				t.Errorf("varroa_controller_external_url = %v, want %q", got["ext"], tt.wantExtURL)
			}
			if got["prefix"] != tt.wantPrefix {
				t.Errorf("varroa_controller_path_prefix = %v, want %q", got["prefix"], tt.wantPrefix)
			}
			if got["ep"] != tt.wantEp {
				t.Errorf("varroa_controller_endpoint = %v, want %q", got["ep"], tt.wantEp)
			}
		})
	}
}

func TestPathModeInjectsLocationURL(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: "varroa.example.com", Mode: v1alpha1.RoutingModePath}

	mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	got := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(mat.JenkinsYAML), &got); err != nil {
		t.Fatalf("parse resolved jenkins.yaml: %v", err)
	}
	unclassified, _ := got["unclassified"].(map[string]interface{})
	if unclassified == nil {
		t.Fatal("unclassified block not injected")
	}
	location, _ := unclassified["location"].(map[string]interface{})
	if location == nil {
		t.Fatal("unclassified.location not injected")
	}
	want := "https://varroa.example.com/jenkins/ns1/test/"
	if location["url"] != want {
		t.Errorf("location.url = %v, want %q", location["url"], want)
	}
}

func TestSubdomainModeInjectsLocationURL(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: "ci.example.com"}

	mat, _, _, err := rec.resolveBundleForController(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveBundleForController: %v", err)
	}

	got := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(mat.JenkinsYAML), &got); err != nil {
		t.Fatalf("parse resolved jenkins.yaml: %v", err)
	}
	unclassified, _ := got["unclassified"].(map[string]interface{})
	if unclassified == nil {
		t.Fatal("unclassified block not injected")
	}
	location, _ := unclassified["location"].(map[string]interface{})
	if location == nil {
		t.Fatal("unclassified.location not injected")
	}
	want := "https://ci.example.com/"
	if location["url"] != want {
		t.Errorf("location.url = %v, want %q", location["url"], want)
	}
}

func TestReconcileIngress_AnnotationsMergePrecedence(t *testing.T) {
	client := newTestClientWithBundle()
	client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
		Spec: v1alpha1.ProvisioningDefaultsSpec{
			IngressAnnotations: map[string]string{"a": "cluster-a", "b": "cluster-b"},
		},
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{
		Host:        "ci.example.com",
		Annotations: map[string]string{"b": "controller-b", "c": "controller-c"},
	}
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(client.ingressCalls) == 0 {
		t.Fatal("Ingress should be created")
	}
	got := client.ingressCalls[0].annotations
	want := map[string]string{"a": "cluster-a", "b": "controller-b", "c": "controller-c"}
	if len(got) != len(want) {
		t.Fatalf("annotations = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("annotations[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestReconcileIngress_IngressClassNameOverride(t *testing.T) {
	client := newTestClientWithBundle()
	client.provisioningDefaults = &v1alpha1.ProvisioningDefaults{
		Spec: v1alpha1.ProvisioningDefaultsSpec{
			IngressClassName: "nginx",
		},
	}
	rec := newTestReconciler(client)

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{
		Host:             "ci.example.com",
		IngressClassName: "traefik",
	}
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(client.ingressCalls) == 0 {
		t.Fatal("Ingress should be created")
	}
	if got := client.ingressCalls[0].ingressClass; got != "traefik" {
		t.Errorf("ingressClass = %q, want traefik (per-controller override)", got)
	}
}

func TestSharedHostMismatchCondition(t *testing.T) {
	findCondition := func(cr *v1alpha1.Controller) *v1alpha1.ControllerCondition {
		for i := range cr.Status.Conditions {
			c := &cr.Status.Conditions[i]
			if c.Type == v1alpha1.ConditionDegraded && c.Reason == "SharedHostMismatch" {
				return c
			}
		}
		return nil
	}

	t.Run("mismatched host sets Degraded", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		rec.SetVarroaRedirectURL("https://dash.example.com/auth/callback")

		cr := pathModeController("other.example.com")
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		cond := findCondition(cr)
		if cond == nil {
			t.Fatal("expected Degraded/SharedHostMismatch condition")
		}
		if !strings.Contains(cond.Message, "other.example.com") || !strings.Contains(cond.Message, "dash.example.com") {
			t.Errorf("condition message missing hosts: %q", cond.Message)
		}
	})

	t.Run("matching host has no condition and clears stale one", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		rec.SetVarroaRedirectURL("https://dash.example.com/auth/callback")

		cr := pathModeController("dash.example.com")
		// Seed a stale mismatch condition to prove it gets cleared.
		cr.Status.Conditions = setCondition(cr.Status.Conditions, v1alpha1.ControllerCondition{
			Type:   v1alpha1.ConditionDegraded,
			Reason: "SharedHostMismatch",
		})
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if cond := findCondition(cr); cond != nil {
			t.Errorf("expected no SharedHostMismatch condition, got %+v", cond)
		}
	})

	t.Run("subdomain controller never gets the condition", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		rec.SetVarroaRedirectURL("https://dash.example.com/auth/callback")

		cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: "ci.example.com"}
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if cond := findCondition(cr); cond != nil {
			t.Errorf("expected no SharedHostMismatch condition, got %+v", cond)
		}
	})
}
