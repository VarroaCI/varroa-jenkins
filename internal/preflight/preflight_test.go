package preflight

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
)

type fakeDeps struct {
	*crdstore.Fake
	defaults  *v1alpha1.ProvisioningDefaults
	quotas    []corev1.ResourceQuota
	hosts     map[string][]string
	quotasErr error
	hostsErr  error
}

func newFakeDeps() *fakeDeps {
	return &fakeDeps{Fake: crdstore.NewFake(), hosts: make(map[string][]string)}
}

func (f *fakeDeps) ListResourceQuotas(_ context.Context, _ string) ([]corev1.ResourceQuota, error) {
	return f.quotas, f.quotasErr
}
func (f *fakeDeps) ListIngressHosts(_ context.Context) (map[string][]string, error) {
	return f.hosts, f.hostsErr
}
func (f *fakeDeps) GetNamespace(_ context.Context, name string) (*corev1.Namespace, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, name)
}

func TestRunReturnsAllChecks(t *testing.T) {
	deps := newFakeDeps()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-name", Namespace: "ns"},
	}
	checks := Run(context.Background(), deps, cr, nil, Options{})
	if len(checks) != 8 {
		t.Fatalf("expected 8 checks, got %d", len(checks))
	}
	ids := []string{"name", "bundle", "version", "pluginCoreCompat", "quota", "ingress-host", "rbac", "namespace"}
	for i, c := range checks {
		if c.ID != ids[i] {
			t.Errorf("check %d: expected id %q, got %q", i, ids[i], c.ID)
		}
		if c.Status != "pass" && c.Status != "warn" && c.Status != "fail" {
			t.Errorf("check %d: invalid status %q", i, c.Status)
		}
		if c.Message == "" {
			t.Errorf("check %d: empty message", i)
		}
	}
}

// Path mode shares the dashboard host by design (#528), so the availability
// check must not flag it as already claimed by the dashboard's own ingress.
func TestCheckIngressHost_PathModeSkipsAvailability(t *testing.T) {
	deps := newFakeDeps()
	deps.hosts = map[string][]string{"varroa.example.com": {"varroa-system/varroa-ingress"}}
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "valid-name", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			IngressSpec: &v1alpha1.IngressSpec{Mode: "path", Host: "varroa.example.com"},
		},
	}
	c := checkIngressHost(context.Background(), deps, cr, nil)
	if c.Status != "pass" {
		t.Errorf("expected pass, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckName_DNSInvalid(t *testing.T) {
	deps := newFakeDeps()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "INVALID_NAME", Namespace: "ns"},
	}
	c := checkName(context.Background(), deps, cr, Options{})
	if c.Status != "fail" {
		t.Errorf("expected fail, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckName_Collision(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "ns"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "ns"},
	}
	c := checkName(context.Background(), deps, cr, Options{})
	if c.Status != "fail" {
		t.Errorf("expected fail, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckName_CollisionForUpdate(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "ns"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", Namespace: "ns"},
	}
	c := checkName(context.Background(), deps, cr, Options{ForUpdate: true})
	if c.Status != "pass" {
		t.Errorf("expected pass for update, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckVersion_Allowed(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-1"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3", Channel: "lts"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.479.3"},
	}
	c := checkVersion(context.Background(), deps, cr, Options{})
	if c.Status != "pass" {
		t.Errorf("expected pass, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckVersion_LineMatch(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-1"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.479", Channel: "lts"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.479.3"},
	}
	c := checkVersion(context.Background(), deps, cr, Options{})
	if c.Status != "pass" {
		t.Errorf("expected pass, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckVersion_WarnOnUnmatched(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "profile-1"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3", Channel: "lts"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.999.0"},
	}
	c := checkVersion(context.Background(), deps, cr, Options{})
	if c.Status != "warn" {
		t.Errorf("expected warn, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckVersion_Empty(t *testing.T) {
	deps := newFakeDeps()
	deps.defaults = &v1alpha1.ProvisioningDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
		Spec:       v1alpha1.ProvisioningDefaultsSpec{DefaultVersion: "2.570"},
	}
	crdstore.MustSeed(deps.Fake, deps.defaults)
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
	}
	c := checkVersion(context.Background(), deps, cr, Options{})
	if c.Status != "pass" {
		t.Errorf("expected pass, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckVersion_LTS(t *testing.T) {
	deps := newFakeDeps()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "lts"},
	}
	c := checkVersion(context.Background(), deps, cr, Options{})
	if c.Status != "pass" {
		t.Errorf("expected pass, got %s: %s", c.Status, c.Message)
	}
}

// versionBelow returns a version string one patch below the given one.
func versionBelow(v string) string {
	cv, ok := jenkinsver.Core(v)
	if !ok || len(cv) == 0 {
		return v
	}
	cv[len(cv)-1]--
	out := fmt.Sprintf("%d", cv[0])
	for i := 1; i < len(cv); i++ {
		out = fmt.Sprintf("%s.%d", out, cv[i])
	}
	return out
}

// versionAbove returns a version string one patch above the given one.
func versionAbove(v string) string {
	cv, ok := jenkinsver.Core(v)
	if !ok || len(cv) == 0 {
		return v
	}
	cv[len(cv)-1]++
	out := fmt.Sprintf("%d", cv[0])
	for i := 1; i < len(cv); i++ {
		out = fmt.Sprintf("%s.%d", out, cv[i])
	}
	return out
}

func TestCheckVersion_Table(t *testing.T) {
	baseline := pluginlock.Baseline()
	if baseline == "" {
		t.Fatal("pluginlock.Baseline() returned empty")
	}
	below := versionBelow(baseline)
	above := versionAbove(baseline)

	tests := []struct {
		name       string
		version    string
		profiles   []*v1alpha1.JenkinsVersionProfile
		forUpdate  bool
		priorVer   string
		wantStatus string
	}{
		// V1: empty
		{name: "V1 empty", version: "", wantStatus: "pass"},
		// V2: lts
		{name: "V2 lts", version: "lts", wantStatus: "pass"},
		// V3: exact match
		{name: "V3 exact", version: "2.479.3", profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
		}, wantStatus: "pass"},
		// V4: line match
		{name: "V4 line", version: "2.479.3", profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479"}},
		}, wantStatus: "pass"},
		// V5: unmatched >= baseline warn
		{name: "V5 unmatched above", version: above, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
		}, wantStatus: "warn"},
		// V6: unmatched < baseline fail
		{name: "V6 unmatched below", version: below, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
		}, wantStatus: "fail"},
		// V7: unparseable fail
		{name: "V7 unparseable", version: "fancy", profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
		}, wantStatus: "fail"},
		// V9: zero profiles, above -> pass
		{name: "V9 zero profiles above", version: above, wantStatus: "pass"},
		// V9: zero profiles, below -> fail
		{name: "V9 zero profiles below", version: below, wantStatus: "fail"},
		// V9: zero profiles, unparseable -> fail
		{name: "V9 zero profiles unparseable", version: "fancy", wantStatus: "fail"},
		// ForUpdate leniency: below baseline + same prior version -> warn
		{name: "ForUpdate same version below", version: below, forUpdate: true, priorVer: below, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
		}, wantStatus: "warn"},
		// ForUpdate leniency: below baseline + different prior version -> fail
		{name: "ForUpdate diff version below", version: below, forUpdate: true, priorVer: above, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3"}},
		}, wantStatus: "fail"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := newFakeDeps()
			for i, p := range tc.profiles {
				p.ObjectMeta = metav1.ObjectMeta{Name: fmt.Sprintf("profile-%d", i)}
				crdstore.MustSeed(deps.Fake, p)
			}
			cr := &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
				Spec:       v1alpha1.ControllerSpec{Version: tc.version},
			}
			c := checkVersion(context.Background(), deps, cr, Options{ForUpdate: tc.forUpdate, PriorVersion: tc.priorVer})
			if c.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q; msg=%q", c.Status, tc.wantStatus, c.Message)
			}
		})
	}
}

func TestCheckPluginCoreCompat_Table(t *testing.T) {
	baseline := pluginlock.Baseline()
	if baseline == "" {
		t.Fatal("pluginlock.Baseline() returned empty")
	}
	below := versionBelow(baseline)
	above := versionAbove(baseline)

	tests := []struct {
		name       string
		version    string
		profiles   []*v1alpha1.JenkinsVersionProfile
		forUpdate  bool
		priorVer   string
		wantStatus string
	}{
		// P1: floating
		{name: "P1 empty", version: "", wantStatus: "pass"},
		{name: "P1 lts", version: "lts", wantStatus: "pass"},
		// P2: matched + ready -> pass
		{name: "P2 matched ready above", version: above, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: above}, Status: v1alpha1.JenkinsVersionProfileStatus{Conditions: []v1alpha1.JenkinsVersionProfileCondition{{Type: "PluginSetReady", Status: metav1.ConditionTrue}}}},
		}, wantStatus: "pass"},
		// P3: matched not-ready (metadata-only) above -> warn
		{name: "P3 metadata-only above", version: above, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: above}},
		}, wantStatus: "warn"},
		// P4: matched not-ready below -> fail
		{name: "P4 metadata-only below", version: below, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: below}},
		}, wantStatus: "fail"},
		// P5: no profile above -> pass
		{name: "P5 no profile above", version: above, wantStatus: "pass"},
		// P6: no profile below -> fail
		{name: "P6 no profile below", version: below, wantStatus: "fail"},
		// P7: unparseable no profile -> warn
		{name: "P7 unparseable", version: "fancy", wantStatus: "warn"},
		// ForUpdate leniency: below + same prior -> warn
		{name: "ForUpdate same version below", version: below, forUpdate: true, priorVer: below, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: below}},
		}, wantStatus: "warn"},
		// ForUpdate leniency: below + different prior -> fail
		{name: "ForUpdate diff version below", version: below, forUpdate: true, priorVer: above, profiles: []*v1alpha1.JenkinsVersionProfile{
			{Spec: v1alpha1.JenkinsVersionProfileSpec{Version: below}},
		}, wantStatus: "fail"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			deps := newFakeDeps()
			for i, p := range tc.profiles {
				p.ObjectMeta = metav1.ObjectMeta{Name: fmt.Sprintf("profile-%d", i)}
				crdstore.MustSeed(deps.Fake, p)
			}
			cr := &v1alpha1.Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
				Spec:       v1alpha1.ControllerSpec{Version: tc.version},
			}
			c := checkPluginCoreCompat(context.Background(), deps, cr, Options{ForUpdate: tc.forUpdate, PriorVersion: tc.priorVer})
			if c.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q; msg=%q", c.Status, tc.wantStatus, c.Message)
			}
		})
	}
}

func TestCheckBundle_ReferencedNotFound(t *testing.T) {
	deps := newFakeDeps()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "missing-bundle"},
		},
	}
	c := checkBundle(context.Background(), deps, cr, nil, Options{OperatorNamespace: "varroa-system"})
	// Missing bundles warn instead of fail — the handler validates existence.
	if c.Status != "warn" {
		t.Errorf("expected warn, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckBundle_ReferencedCrossNamespace(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "test-bundle", Namespace: "other-ns"},
		Status:     v1alpha1.ComposedBundleStatus{Phase: v1alpha1.ComposedBundleReady, ResolvedHash: "abc123"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "test-bundle", Namespace: "other-ns"},
		},
	}
	c := checkBundle(context.Background(), deps, cr, nil, Options{OperatorNamespace: "varroa-system"})
	if c.Status != "pass" {
		t.Errorf("expected pass, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckRBAC_UnknownRole(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "admin", Labels: map[string]string{v1alpha1.LabelBuiltin: "true"}},
		Spec:       v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "varroa-admin"},
	})
	crdstore.MustSeed(deps.Fake, &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer", Labels: map[string]string{v1alpha1.LabelBuiltin: "true"}},
		Spec:       v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "varroa-viewer"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			RBACSpec: &v1alpha1.RBACSpec{
				Groups: []v1alpha1.RBACGroupBinding{
					{Name: "devs", Role: "superuser"},
				},
			},
		},
	}
	c := checkRBAC(context.Background(), deps, cr, Options{})
	if c.Status != "fail" {
		t.Errorf("expected fail, got %s: %s", c.Status, c.Message)
	}
}

func TestCheckRBAC_EmptyGroup(t *testing.T) {
	deps := newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.VarroaRole{
		ObjectMeta: metav1.ObjectMeta{Name: "viewer", Labels: map[string]string{v1alpha1.LabelBuiltin: "true"}},
		Spec:       v1alpha1.VarroaRoleSpec{JenkinsRoleRef: "varroa-viewer"},
	})
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			RBACSpec: &v1alpha1.RBACSpec{
				Groups: []v1alpha1.RBACGroupBinding{
					{Name: "empty-group", Role: "viewer"},
				},
			},
		},
	}
	c := checkRBAC(context.Background(), deps, cr, Options{})
	if c.Status != "warn" {
		t.Errorf("expected warn, got %s: %s", c.Status, c.Message)
	}
}

// A draft with no composedBundleRef used to report a bare "no bundle
// referenced" pass, hiding the one thing that can go wrong on the zero-config
// path: the operator has not finished seeding the starter, or its compose
// failed. The wizard shows this check, so a silent pass there is a silent
// failure later.
func TestCheckBundle_NilRefChecksTheStarterBundle(t *testing.T) {
	ctx := context.Background()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "team-a"},
	}
	opts := Options{OperatorNamespace: "varroa-system"}

	// Not seeded yet.
	deps := newFakeDeps()
	got := checkBundle(ctx, deps, cr, nil, opts)
	if got.Status != "warn" {
		t.Fatalf("status = %q, want warn when the starter is absent", got.Status)
	}
	if !strings.Contains(got.Message, v1alpha1.StarterBundleName) {
		t.Errorf("message should name the starter bundle, got %q", got.Message)
	}

	// Seeded but not Ready.
	deps = newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.StarterBundleName, Namespace: "varroa-system"},
		Status:     v1alpha1.ComposedBundleStatus{Phase: v1alpha1.ComposedBundlePending},
	})
	if got := checkBundle(ctx, deps, cr, nil, opts); got.Status != "warn" {
		t.Errorf("status = %q, want warn when the starter is not Ready", got.Status)
	}

	// Seeded and Ready.
	deps = newFakeDeps()
	crdstore.MustSeed(deps.Fake, &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.StarterBundleName, Namespace: "varroa-system"},
		Status: v1alpha1.ComposedBundleStatus{
			Phase:        v1alpha1.ComposedBundleReady,
			ResolvedHash: "abc123",
		},
	})
	got = checkBundle(ctx, deps, cr, nil, opts)
	if got.Status != "pass" {
		t.Fatalf("status = %q, want pass when the starter is Ready", got.Status)
	}
	if !strings.Contains(got.Message, "starter") {
		t.Errorf("a passing zero-config check should say which bundle it will use, got %q", got.Message)
	}
}
