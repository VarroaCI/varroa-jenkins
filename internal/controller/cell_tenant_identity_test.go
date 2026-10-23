package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestApplyCellContent_ProjectsTenantAgentTokenAsSystemCredential is task
// 8.9's "the credential" acceptance clause: once the tenant agent's token
// Secret (created by the placement pass) carries a token value, the
// controller reconciler reads it and carries it on
// mat.CellCloudCredentials as the SYSTEM-scope secret-text credential
// varroa-cloud-<tenant> — without creating or modifying the ServiceAccount,
// its token Secret, or its RBAC. The controller reconciler only reads what
// the placement pass owns.
//
// mat.JenkinsYAML — the content the CASC ConfigMap write reads — carries no
// credential value (design D8): applyCellContent hands the credential to
// the caller through the separate field precisely so no code path that
// serializes mat.JenkinsYAML into a ConfigMap can pick it up.
// buildDesiredStateCommand is the only place that injects it, onto the
// mite-bound JcascYaml (TestBuildDesiredStateCommand_TenantCloudCredential
// covers that seam).
func TestApplyCellContent_ProjectsTenantAgentTokenAsSystemCredential(t *testing.T) {
	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        "jenkins-lts",
				AgentNamespace: "payments-ns",
			},
		},
	})
	// Simulate the placement pass's already-created, already-populated
	// token Secret.
	client.existingSecrets["varroa-agent-payments-token"] = map[string][]byte{
		"token": []byte("tok-abc123"),
	}

	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	mat := &bundle.MaterializedBundle{JenkinsYAML: ""}

	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}

	if len(mat.CellCloudCredentials) != 1 {
		t.Fatalf("expected exactly one CellCloudCredentials entry, got %+v", mat.CellCloudCredentials)
	}
	cred := mat.CellCloudCredentials[0]
	if cred.ID != "varroa-cloud-payments" || cred.Secret != "tok-abc123" {
		t.Errorf("unexpected credential: %+v, want ID=varroa-cloud-payments Secret=tok-abc123", cred)
	}
	if strings.Contains(mat.JenkinsYAML, "tok-abc123") || strings.Contains(mat.JenkinsYAML, "credentials:") {
		t.Errorf("expected no credential value in mat.JenkinsYAML (it feeds the CASC ConfigMap), got:\n%s", mat.JenkinsYAML)
	}

	// The controller reconciler is not the owner: it must not create or
	// touch the ServiceAccount, its token Secret, or its RBAC.
	if len(client.serviceAccounts) != 0 {
		t.Errorf("controller reconciler must not create a ServiceAccount, got %v", client.serviceAccounts)
	}
	if len(client.saTokenSecrets) != 0 {
		t.Errorf("controller reconciler must not create a token Secret, got %v", client.saTokenSecrets)
	}
	if len(client.agentRBACCalls) != 0 {
		t.Errorf("controller reconciler must not create agent RBAC, got %v", client.agentRBACCalls)
	}
}

// TestApplyCellContent_MissingTokenSkipsCredentialInjection asserts a
// tenant agent token Secret whose token key is not populated yet (the
// token controller has not caught up) is skipped, not an error.
func TestApplyCellContent_MissingTokenSkipsCredentialInjection(t *testing.T) {
	client := newTestClient()
	crdstore.MustSeed(client.store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        "jenkins-lts",
				AgentNamespace: "payments-ns",
			},
		},
	})
	// No existingSecrets entry: GetSecret returns (nil, nil) in the fake.

	rec := newTestReconciler(client)
	cr := cellControllerFixture()
	cr.Status.Cell = &v1alpha1.CellStatus{Tenants: []string{"payments"}}
	mat := &bundle.MaterializedBundle{JenkinsYAML: ""}

	if err := rec.applyCellContent(context.Background(), cr, mat, nil); err != nil {
		t.Fatalf("applyCellContent: %v", err)
	}
	// The cloud still names credentialsId varroa-cloud-payments (it always
	// does — that is what "SYSTEM-scope credential" means to the cloud),
	// but no credentials.system.domainCredentials entry actually supplies
	// it while the token key is unpopulated.
	if strings.Contains(mat.JenkinsYAML, "credentials:") {
		t.Errorf("expected no credentials.system.domainCredentials entry while the token is unpopulated, got:\n%s", mat.JenkinsYAML)
	}
}

// TestCellTenantCloudCredentialNeverReachesConfigMapButReachesMite is
// defect 4 / task 8.11's full-reconcile acceptance (design D8: "No secret
// value enters a ConfigMap"): a connected-phase reconcile of a cell with
// one placed tenant writes the tenant's cloud token into no ConfigMap the
// operator creates or updates, and into the JcascYaml of the desired-state
// command it pushes to the mite.
func TestCellTenantCloudCredentialNeverReachesConfigMapButReachesMite(t *testing.T) {
	client := newTestClientWithBundle()
	crdstore.MustSeed(client.store, &v1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TenantSpec{
			Namespaces: []string{"payments-ns"},
			Pipelines: &v1alpha1.TenantPipelinesSpec{
				Profile:        "jenkins-lts",
				AgentNamespace: "payments-ns",
			},
		},
	})
	const secretValue = "tok-cloud-secret-xyz"
	client.existingSecrets["varroa-agent-payments-token"] = map[string][]byte{
		"token": []byte(secretValue),
	}

	rec := newTestReconcilerWithTokenSigner(client)
	transport := &captureTransport{}
	rec.miteTransport = transport

	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"},
			Cell:              &v1alpha1.CellSpec{Enabled: true},
		},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
			Cell:       &v1alpha1.CellStatus{Tenants: []string{"payments"}},
		},
	}

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}

	if transport.lastDesired == nil || !strings.Contains(transport.lastDesired.JcascYaml, secretValue) {
		t.Fatalf("expected the desired-state command's JcascYaml to carry the credential, got: %+v", transport.lastDesired)
	}
	for name, data := range client.configMapData {
		for key, val := range data {
			if strings.Contains(val, secretValue) {
				t.Errorf("credential value leaked into ConfigMap %q key %q", name, key)
			}
		}
	}
}
