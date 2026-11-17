package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func TestCascSeedData(t *testing.T) {
	withRBAC := cascSeedData(&bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}, "rbac-doc")
	if withRBAC[cascSeedRealmKey] != generateRealmDocument() {
		t.Errorf("realm.yaml = %q, want the generated realm document", withRBAC[cascSeedRealmKey])
	}
	if !strings.Contains(withRBAC[cascSeedConfigKey], "systemMessage: hi") ||
		!strings.Contains(withRBAC[cascSeedConfigKey], "projectNamingStrategy") {
		t.Errorf("config.yaml = %q, want the bundle with the project naming strategy", withRBAC[cascSeedConfigKey])
	}
	if withRBAC[cascSeedRBACKey] != "rbac-doc" {
		t.Errorf("rbac.yaml = %q, want rbac-doc", withRBAC[cascSeedRBACKey])
	}

	noBundle := cascSeedData(nil, "")
	if v, ok := noBundle[cascSeedConfigKey]; !ok || v != "" {
		t.Errorf("config.yaml = %q (present %v) for a nil bundle, want an empty value", v, ok)
	}
	if _, ok := noBundle[cascSeedRBACKey]; ok {
		t.Error("rbac.yaml is present for an empty RBAC document, want it left out")
	}
}

func TestCascSeedVerdict(t *testing.T) {
	desired := &mitev1.DesiredStateCommand{JcascYaml: "cfg", RbacYaml: "rbac", DesiredStateHash: "h1"}
	seed := map[string]string{cascSeedConfigKey: "cfg-seed", cascSeedRBACKey: "rbac-seed"}
	cfgHash := sha256Hex([]byte("cfg-seed"))
	rbacHash := sha256Hex([]byte("rbac-seed"))

	cases := []struct {
		name        string
		res         *mitev1.DesiredStateResult
		desired     *mitev1.DesiredStateCommand
		wantOK      bool
		wantRefusal *v1alpha1.CascRefusal
	}{
		{name: "no result", res: nil, desired: desired},
		{name: "result for another desired state", desired: desired,
			res: &mitev1.DesiredStateResult{AppliedHash: "h0", ConfigSuccess: true, RbacSuccess: true}},
		{name: "accepted", desired: desired, wantOK: true,
			res: &mitev1.DesiredStateResult{AppliedHash: "h1", ConfigSuccess: true, RbacSuccess: true}},
		{name: "refused reload marks both documents", desired: desired, wantOK: true,
			res: &mitev1.DesiredStateResult{AppliedHash: "h1",
				ConfigError: "reload failed: bad; rolled back to last-good",
				RbacError:   "reload failed: bad; rolled back to last-good"},
			wantRefusal: &v1alpha1.CascRefusal{ConfigHash: cfgHash, RBACHash: rbacHash}},
		{name: "refused rbac only", desired: desired, wantOK: true,
			res:         &mitev1.DesiredStateResult{AppliedHash: "h1", ConfigSuccess: true, RbacError: "bad pattern"},
			wantRefusal: &v1alpha1.CascRefusal{RBACHash: rbacHash}},
		{name: "jenkins not ready gives no verdict", desired: desired,
			res: &mitev1.DesiredStateResult{AppliedHash: "h1"}},
		{name: "no rbac document needs no rbac success",
			desired: &mitev1.DesiredStateCommand{JcascYaml: "cfg", DesiredStateHash: "h1"}, wantOK: true,
			res: &mitev1.DesiredStateResult{AppliedHash: "h1", ConfigSuccess: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			refusal, ok := cascSeedVerdict(tc.res, tc.desired, seed)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			switch {
			case tc.wantRefusal == nil && refusal != nil:
				t.Fatalf("refusal = %+v, want nil", refusal)
			case tc.wantRefusal != nil && (refusal == nil || *refusal != *tc.wantRefusal):
				t.Fatalf("refusal = %+v, want %+v", refusal, tc.wantRefusal)
			}
		})
	}
}

func TestCascSeedWithoutRefused(t *testing.T) {
	current := map[string]string{
		cascSeedRealmKey:  "realm-new",
		cascSeedConfigKey: "cfg-refused",
		cascSeedRBACKey:   "rbac-new",
	}
	existing := map[string]string{
		cascSeedRealmKey:  "realm-old",
		cascSeedConfigKey: "cfg-accepted",
		cascSeedRBACKey:   "rbac-accepted",
	}

	t.Run("nil refusal copies current", func(t *testing.T) {
		got := cascSeedWithoutRefused(current, existing, nil)
		if got[cascSeedConfigKey] != "cfg-refused" || got[cascSeedRBACKey] != "rbac-new" {
			t.Fatalf("seed = %v, want current", got)
		}
		got[cascSeedConfigKey] = "mutated"
		if current[cascSeedConfigKey] != "cfg-refused" {
			t.Fatal("the answer aliases current")
		}
	})

	t.Run("refused document keeps its existing value", func(t *testing.T) {
		refusal := &v1alpha1.CascRefusal{ConfigHash: sha256Hex([]byte("cfg-refused"))}
		got := cascSeedWithoutRefused(current, existing, refusal)
		if got[cascSeedConfigKey] != "cfg-accepted" {
			t.Errorf("config.yaml = %q, want cfg-accepted", got[cascSeedConfigKey])
		}
		if got[cascSeedRBACKey] != "rbac-new" {
			t.Errorf("rbac.yaml = %q, want rbac-new", got[cascSeedRBACKey])
		}
		if got[cascSeedRealmKey] != "realm-new" {
			t.Errorf("realm.yaml = %q, want realm-new", got[cascSeedRealmKey])
		}
	})

	t.Run("changed document is seeded", func(t *testing.T) {
		refusal := &v1alpha1.CascRefusal{
			ConfigHash: sha256Hex([]byte("cfg-older-refused")),
			RBACHash:   sha256Hex([]byte("rbac-new")),
		}
		got := cascSeedWithoutRefused(current, existing, refusal)
		if got[cascSeedConfigKey] != "cfg-refused" {
			t.Errorf("config.yaml = %q, want the changed current value", got[cascSeedConfigKey])
		}
		if got[cascSeedRBACKey] != "rbac-accepted" {
			t.Errorf("rbac.yaml = %q, want rbac-accepted", got[cascSeedRBACKey])
		}
	})

	t.Run("refused document with no existing value is left out", func(t *testing.T) {
		refusal := &v1alpha1.CascRefusal{RBACHash: sha256Hex([]byte("rbac-new"))}
		got := cascSeedWithoutRefused(current, map[string]string{cascSeedConfigKey: "cfg-accepted"}, refusal)
		if _, ok := got[cascSeedRBACKey]; ok {
			t.Errorf("rbac.yaml = %q, want it left out", got[cascSeedRBACKey])
		}
		got = cascSeedWithoutRefused(current, nil, refusal)
		if _, ok := got[cascSeedRBACKey]; ok {
			t.Errorf("rbac.yaml = %q with no seed, want it left out", got[cascSeedRBACKey])
		}
	})
}

// TestHandleConnected_CascSeedFollowsLiveVerdict drives the next-boot seed
// through a push, an accepted verdict, a new push, and a refused verdict. The
// seed never holds content before a verdict, and it never takes a refused
// config.yaml.
func TestHandleConnected_CascSeedFollowsLiveVerdict(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	tr := &captureTransport{}
	rec.miteTransport = tr
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now())},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
		},
	}
	seedName := cascConfigMapName(testPrefix)
	tick := func(results ...*mitev1.CommandResult) {
		t.Helper()
		tr.drainResults = results
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		tr.drainResults = nil
	}

	tick()
	if tr.lastDesired == nil {
		t.Fatal("expected a desired-state push")
	}
	if _, ok := client.configMapData[seedName]; ok {
		t.Fatal("the seed was written before the mite gave a verdict")
	}

	tick(mkDSR(true, true, true, true, "", "", "", "", tr.lastDesired.DesiredStateHash))
	seed := client.configMapData[seedName]
	if !strings.Contains(seed[cascSeedConfigKey], "Hello Varroa") {
		t.Fatalf("config.yaml = %q after an accepted verdict, want the pushed bundle", seed[cascSeedConfigKey])
	}
	if cr.Status.CascRefusal != nil {
		t.Fatalf("cascRefusal = %+v after an accepted verdict, want nil", cr.Status.CascRefusal)
	}

	client.configMapData["test-bundle-content"]["jenkins.yaml"] = "jenkins:\n  systemMessage: \"Refused\""
	accepted := tr.lastDesired.DesiredStateHash
	tick()
	if tr.lastDesired.DesiredStateHash == accepted {
		t.Fatal("expected a push of the changed bundle")
	}
	const reloadErr = "reload failed: boom; rolled back to last-good"
	tick(mkDSR(false, false, true, true, reloadErr, reloadErr, "", "", tr.lastDesired.DesiredStateHash))
	seed = client.configMapData[seedName]
	if strings.Contains(seed[cascSeedConfigKey], "Refused") || !strings.Contains(seed[cascSeedConfigKey], "Hello Varroa") {
		t.Fatalf("config.yaml = %q after a refused verdict, want the accepted bundle", seed[cascSeedConfigKey])
	}
	if cr.Status.CascRefusal == nil || cr.Status.CascRefusal.ConfigHash == "" {
		t.Fatalf("cascRefusal = %+v after a refused verdict, want the config.yaml digest", cr.Status.CascRefusal)
	}
	if body := controllerStatusPatch(&cr.Status)["cascRefusal"]; body == nil {
		t.Fatal("the status patch does not carry cascRefusal")
	}

	// A pod roll after the refusal computes the same digest, so it keeps the
	// accepted config.yaml too.
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	tick()
	seed = client.configMapData[seedName]
	if strings.Contains(seed[cascSeedConfigKey], "Refused") || !strings.Contains(seed[cascSeedConfigKey], "Hello Varroa") {
		t.Fatalf("config.yaml = %q after a pod roll, want the accepted bundle", seed[cascSeedConfigKey])
	}
}

// TestHandleProvisioning_CascSeedKeepsRefusedDocument proves that a pod roll
// does not seed a config.yaml that the live reload refused, and that the
// pod-template hash describes the seed that the ConfigMap holds.
func TestHandleProvisioning_CascSeedKeepsRefusedDocument(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)
	seedName := cascConfigMapName(testPrefix)

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("first provisioning pass: %v", err)
	}
	current := client.configMapData[seedName][cascSeedConfigKey]
	if current == "" {
		t.Fatal("the first provisioning pass wrote no config.yaml")
	}

	client.configMapData[seedName] = map[string]string{
		cascSeedRealmKey:  "realm-old",
		cascSeedConfigKey: "jenkins:\n  systemMessage: accepted\n",
	}
	cr.Status.Phase = v1alpha1.ControllerPhaseProvisioning
	cr.Status.CascRefusal = &v1alpha1.CascRefusal{ConfigHash: sha256Hex([]byte(current))}
	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("second provisioning pass: %v", err)
	}
	seed := client.configMapData[seedName]
	if seed[cascSeedConfigKey] != "jenkins:\n  systemMessage: accepted\n" {
		t.Fatalf("config.yaml = %q, want the accepted seed value", seed[cascSeedConfigKey])
	}
	if seed[cascSeedRealmKey] != generateRealmDocument() {
		t.Errorf("realm.yaml = %q, want the generated realm document", seed[cascSeedRealmKey])
	}
	last := client.stsSpecs[len(client.stsSpecs)-1]
	if last.CascContentHash != cascContentHash(seed) {
		t.Errorf("pod-template casc hash = %q, want the hash of the written seed %q", last.CascContentHash, cascContentHash(seed))
	}
}

// TestHandleConnected_RBACRefusalAdvancesConfigSeed drives a bundle change
// whose push Jenkins accepts for config.yaml and refuses for rbac.yaml. The
// status names the rbac section alone, and the seed takes the new config.yaml.
func TestHandleConnected_RBACRefusalAdvancesConfigSeed(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconcilerWithTokenSigner(client)
	tr := &captureTransport{}
	rec.miteTransport = tr
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test", Namespace: "ns", UID: testUID, CreationTimestamp: metav1.NewTime(time.Now()),
			Annotations: map[string]string{"varroa.dev/allow-admin-lockout": "true"},
		},
		Spec: v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle"}},
		Status: v1alpha1.ControllerStatus{
			Phase:      v1alpha1.ControllerPhaseConnected,
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "MiteConnected"}},
		},
	}
	seedName := cascConfigMapName(testPrefix)
	tick := func(results ...*mitev1.CommandResult) {
		t.Helper()
		tr.drainResults = results
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		tr.drainResults = nil
	}

	tick()
	if tr.lastDesired == nil || tr.lastDesired.RbacYaml == "" {
		t.Fatal("expected a desired-state push with rbac.yaml")
	}
	tick(mkDSR(true, true, true, true, "", "", "", "", tr.lastDesired.DesiredStateHash))
	acceptedRBAC := client.configMapData[seedName][cascSeedRBACKey]
	if acceptedRBAC == "" {
		t.Fatal("the seed holds no rbac.yaml after an accepted verdict")
	}

	client.configMapData["test-bundle-content"]["jenkins.yaml"] = "jenkins:\n  systemMessage: \"Changed\""
	tick()
	const rbacErr = "reload failed: reload config: Jenkins answered its error page: Error loading configuration Unclosed counted closure; rolled back to last-good"
	tick(mkDSR(true, false, true, true, "", rbacErr, "", "", tr.lastDesired.DesiredStateHash))

	ar := cr.Status.LastApplyResult
	if ar == nil || ar.Succeeded {
		t.Fatalf("lastApplyResult = %+v, want a failed apply", ar)
	}
	if config := ar.Sections[0]; config.Name != "config" || !config.OK || config.Error != "" {
		t.Errorf("config section = %+v, want ok with no error", config)
	}
	if rbac := ar.Sections[1]; rbac.Name != "rbac" || rbac.OK || rbac.Error != rbacErr {
		t.Errorf("rbac section = %+v, want the refusal text", rbac)
	}
	if c := findCondition(cr.Status.Conditions, v1alpha1.ConditionConfigApplyFailed); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("ConfigApplyFailed = %+v, want False", c)
	}
	seed := client.configMapData[seedName]
	if !strings.Contains(seed[cascSeedConfigKey], "Changed") {
		t.Errorf("config.yaml = %q, want the accepted new bundle", seed[cascSeedConfigKey])
	}
	if seed[cascSeedRBACKey] != acceptedRBAC {
		t.Errorf("rbac.yaml = %q, want the accepted rbac.yaml", seed[cascSeedRBACKey])
	}
	if r := cr.Status.CascRefusal; r == nil || r.ConfigHash != "" || r.RBACHash == "" {
		t.Errorf("cascRefusal = %+v, want the rbac.yaml digest alone", r)
	}
}
