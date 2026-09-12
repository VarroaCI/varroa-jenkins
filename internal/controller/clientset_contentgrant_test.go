package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// testGrant builds a ContentReadGrant for the tests below.
func testGrant(configMapName string) ContentReadGrant {
	return ContentReadGrant{
		Name:             "demo-content-reader",
		Namespace:        "smoke-team-a",
		ConfigMapName:    configMapName,
		SubjectName:      "varroa-varroa-bff",
		SubjectNamespace: "varroa-system",
		Owner: metav1.OwnerReference{
			APIVersion: "varroa.dev/v1alpha1",
			Kind:       "ComposedBundle",
			Name:       "demo",
			UID:        "bundle-uid",
		},
	}
}

// TestCreateOrUpdateContentReadGrantCreates asserts the created Role grants get
// on exactly one named ConfigMap, that the RoleBinding names the subject, and
// that both objects carry the owner reference and no labels.
func TestCreateOrUpdateContentReadGrantCreates(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()
	g := testGrant("demo-content")

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	role, err := c.clientset.RbacV1().Roles(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("want exactly one rule, got %d", len(role.Rules))
	}
	rule := role.Rules[0]
	if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "" {
		t.Errorf("apiGroups = %v, want [\"\"]", rule.APIGroups)
	}
	if len(rule.Resources) != 1 || rule.Resources[0] != "configmaps" {
		t.Errorf("resources = %v, want [configmaps]", rule.Resources)
	}
	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "demo-content" {
		t.Errorf("resourceNames = %v, want [demo-content]", rule.ResourceNames)
	}
	if len(rule.Verbs) != 1 || rule.Verbs[0] != "get" {
		t.Errorf("verbs = %v, want [get]", rule.Verbs)
	}
	if len(role.OwnerReferences) != 1 || role.OwnerReferences[0].Name != "demo" {
		t.Errorf("role owner references = %v, want one naming demo", role.OwnerReferences)
	}
	if len(role.Labels) != 0 {
		t.Errorf("role labels = %v, want none", role.Labels)
	}

	rb, err := c.clientset.RbacV1().RoleBindings(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rolebinding: %v", err)
	}
	if rb.RoleRef.Kind != "Role" || rb.RoleRef.Name != g.Name {
		t.Errorf("roleRef = %+v, want Role/%s", rb.RoleRef, g.Name)
	}
	if len(rb.Subjects) != 1 {
		t.Fatalf("want exactly one subject, got %d", len(rb.Subjects))
	}
	sub := rb.Subjects[0]
	if sub.Kind != "ServiceAccount" || sub.Name != g.SubjectName || sub.Namespace != g.SubjectNamespace {
		t.Errorf("subject = %+v, want the BFF service account", sub)
	}
	if len(rb.OwnerReferences) != 1 || rb.OwnerReferences[0].Name != "demo" {
		t.Errorf("rolebinding owner references = %v, want one naming demo", rb.OwnerReferences)
	}
	if len(rb.Labels) != 0 {
		t.Errorf("rolebinding labels = %v, want none", rb.Labels)
	}
}

// TestCreateOrUpdateContentReadGrantRewritesResourceNames asserts a later call
// with a different ConfigMap name replaces the resourceNames list. A renamed
// content ConfigMap must not leave the old name readable.
func TestCreateOrUpdateContentReadGrantRewritesResourceNames(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()

	if err := c.CreateOrUpdateContentReadGrant(ctx, testGrant("old-content")); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := c.CreateOrUpdateContentReadGrant(ctx, testGrant("new-content")); err != nil {
		t.Fatalf("second call: %v", err)
	}

	role, err := c.clientset.RbacV1().Roles("smoke-team-a").Get(ctx, "demo-content-reader", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("want exactly one rule, got %d", len(role.Rules))
	}
	names := role.Rules[0].ResourceNames
	if len(names) != 1 || names[0] != "new-content" {
		t.Errorf("resourceNames = %v, want [new-content] only", names)
	}
}

// countVerbs counts the write actions a fake clientset recorded, by verb.
func countVerbs(cs *fake.Clientset, verb string) int {
	n := 0
	for _, a := range cs.Actions() {
		if a.GetVerb() == verb {
			n++
		}
	}
	return n
}

// TestCreateOrUpdateContentReadGrantSecondApplyWritesNothing asserts an
// unchanged grant costs no write. The catalog ticker reconciles every bundle on
// a short interval, so an unconditional update would put steady write load on
// the API server for a spec that never changes.
func TestCreateOrUpdateContentReadGrantSecondApplyWritesNothing(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	g := testGrant("demo-content")

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	creates := countVerbs(cs, "create")
	if creates != 2 {
		t.Fatalf("first apply performed %d creates, want 2 (Role and RoleBinding)", creates)
	}

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if got := countVerbs(cs, "update"); got != 0 {
		t.Errorf("second apply performed %d updates, want 0 for an unchanged grant", got)
	}
	if got := countVerbs(cs, "create"); got != creates {
		t.Errorf("second apply performed %d creates in total, want %d", got, creates)
	}
}

// TestCreateOrUpdateContentReadGrantKeepsForeignMetadata asserts an update
// preserves a label another actor added. The operator owns the rule and the
// owner reference, not the whole object.
func TestCreateOrUpdateContentReadGrantKeepsForeignMetadata(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	g := testGrant("old-content")

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	// A cluster policy tool labels and annotates both objects.
	role, err := cs.RbacV1().Roles(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	role.Labels = map[string]string{"policy.example.com/scanned": "true"}
	role.Annotations = map[string]string{"policy.example.com/at": "now"}
	if _, err := cs.RbacV1().Roles(g.Namespace).Update(ctx, role, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("label role: %v", err)
	}
	rb, err := cs.RbacV1().RoleBindings(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rolebinding: %v", err)
	}
	rb.Labels = map[string]string{"policy.example.com/scanned": "true"}
	if _, err := cs.RbacV1().RoleBindings(g.Namespace).Update(ctx, rb, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("label rolebinding: %v", err)
	}

	// The content ConfigMap is renamed, which forces a real update of the rule.
	if err := c.CreateOrUpdateContentReadGrant(ctx, testGrant("new-content")); err != nil {
		t.Fatalf("reconcile grant: %v", err)
	}

	role, err = cs.RbacV1().Roles(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get role after update: %v", err)
	}
	if got := role.Rules[0].ResourceNames; len(got) != 1 || got[0] != "new-content" {
		t.Errorf("resourceNames = %v, want [new-content]", got)
	}
	if role.Labels["policy.example.com/scanned"] != "true" {
		t.Errorf("role labels = %v, want the foreign label preserved", role.Labels)
	}
	if role.Annotations["policy.example.com/at"] != "now" {
		t.Errorf("role annotations = %v, want the foreign annotation preserved", role.Annotations)
	}

	rb, err = cs.RbacV1().RoleBindings(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rolebinding after update: %v", err)
	}
	if rb.Labels["policy.example.com/scanned"] != "true" {
		t.Errorf("rolebinding labels = %v, want the foreign label preserved", rb.Labels)
	}
}

// TestCreateOrUpdateContentReadGrantConvergesLostCreateRace asserts a create
// that loses a race still converges the winning object in the same call.
//
// Another writer can create a wider Role between the operator's read and its
// create. Treating AlreadyExists as success would leave the BFF bound to that
// wider Role until the next tick, which is a real permission gap.
func TestCreateOrUpdateContentReadGrantConvergesLostCreateRace(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	g := testGrant("demo-content")

	// The wider Role already exists in the tracker, so the create below fails
	// with AlreadyExists on its own. Forcing only the FIRST Get to report
	// NotFound reproduces the race exactly: the operator reads nothing, tries
	// to create, and loses.
	wider := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      g.Name,
			Namespace: g.Namespace,
			Labels:    map[string]string{"other.example.com/owner": "someone-else"},
		},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"configmaps"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}
	if _, err := cs.RbacV1().Roles(g.Namespace).Create(ctx, wider, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed the wider role: %v", err)
	}

	var firstGet bool
	cs.PrependReactor("get", "roles", func(clienttesting.Action) (bool, runtime.Object, error) {
		if firstGet {
			return false, nil, nil
		}
		firstGet = true
		return true, nil, apierrors.NewNotFound(rbacv1.Resource("roles"), g.Name)
	})

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("apply grant: %v", err)
	}

	role, err := cs.RbacV1().Roles(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	if len(role.Rules) != 1 {
		t.Fatalf("want exactly one rule, got %d", len(role.Rules))
	}
	rule := role.Rules[0]
	if len(rule.Verbs) != 1 || rule.Verbs[0] != "get" {
		t.Errorf("verbs = %v, want the operator's [get] after the lost race", rule.Verbs)
	}
	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "demo-content" {
		t.Errorf("resourceNames = %v, want the operator's name-scoped list", rule.ResourceNames)
	}
	if len(role.OwnerReferences) != 1 || role.OwnerReferences[0].Name != "demo" {
		t.Errorf("owner references = %v, want the bundle's own", role.OwnerReferences)
	}
	// The other writer's label is metadata the operator does not own.
	if role.Labels["other.example.com/owner"] != "someone-else" {
		t.Errorf("labels = %v, want the other writer's label preserved", role.Labels)
	}
}

// foreignBinding builds a RoleBinding of the grant's name that points at
// another Role and carries another actor's metadata.
func foreignBinding(g ContentReadGrant) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        g.Name,
			Namespace:   g.Namespace,
			UID:         "foreign-uid",
			Labels:      map[string]string{"policy.example.com/scanned": "true"},
			Annotations: map[string]string{"policy.example.com/at": "now"},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "someone-elses-role",
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      "someone-else",
			Namespace: "other-ns",
		}},
	}
}

// TestCreateOrUpdateContentReadGrantRecreatesOnDifferentRoleRef asserts a
// binding pointing at another Role is replaced, not updated.
//
// roleRef is immutable in Kubernetes. An update can never converge such an
// object, so without a recreate the BFF stays bound to a Role the operator
// does not control, on every tick.
func TestCreateOrUpdateContentReadGrantRecreatesOnDifferentRoleRef(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	g := testGrant("demo-content")

	if _, err := cs.RbacV1().RoleBindings(g.Namespace).Create(ctx, foreignBinding(g), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed the foreign binding: %v", err)
	}
	before := len(cs.Actions())

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("apply grant: %v", err)
	}

	rb, err := cs.RbacV1().RoleBindings(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get rolebinding: %v", err)
	}
	if rb.RoleRef.Name != g.Name {
		t.Errorf("roleRef.name = %q, want the operator's %q", rb.RoleRef.Name, g.Name)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0].Name != g.SubjectName {
		t.Errorf("subjects = %+v, want the BFF service account", rb.Subjects)
	}
	if len(rb.OwnerReferences) != 1 || rb.OwnerReferences[0].Name != "demo" {
		t.Errorf("owner references = %v, want the bundle's own", rb.OwnerReferences)
	}
	// A recreate keeps foreign metadata, exactly as an update does. An
	// admission policy that requires a label would otherwise reject the
	// create, and the BFF would stay unbound.
	if rb.Labels["policy.example.com/scanned"] != "true" {
		t.Errorf("labels = %v, want the foreign label preserved across the recreate", rb.Labels)
	}
	if rb.Annotations["policy.example.com/at"] != "now" {
		t.Errorf("annotations = %v, want the foreign annotation preserved", rb.Annotations)
	}

	var deletes, creates int
	for _, a := range cs.Actions()[before:] {
		if a.GetResource().Resource != "rolebindings" {
			continue
		}
		switch a.GetVerb() {
		case "delete":
			deletes++
		case "create":
			creates++
		}
	}
	if deletes != 1 {
		t.Errorf("rolebinding deletes = %d, want exactly 1", deletes)
	}
	if creates != 1 {
		t.Errorf("rolebinding creates = %d, want exactly 1", creates)
	}

	// The UID precondition is what stops the delete from removing a
	// replacement that appeared after the read. The fake tracker ignores
	// DeleteOptions, so assert it from the recorded action instead.
	var checked bool
	for _, a := range cs.Actions()[before:] {
		da, ok := a.(clienttesting.DeleteActionImpl)
		if !ok || da.GetResource().Resource != "rolebindings" {
			continue
		}
		checked = true
		pre := da.DeleteOptions.Preconditions
		if pre == nil || pre.UID == nil {
			t.Fatalf("delete carried no UID precondition: %+v", da.DeleteOptions)
		}
		if *pre.UID != "foreign-uid" {
			t.Errorf("delete precondition UID = %q, want the observed foreign-uid", *pre.UID)
		}
	}
	if !checked {
		t.Error("no rolebinding delete action was recorded")
	}
}

// TestCreateOrUpdateContentReadGrantLeavesAReplacedBindingAlone asserts a
// failed UID precondition stops the call without a create.
//
// A Conflict means another writer replaced the object after the read. That
// replacement is not the one this call observed, and removing it blindly is
// exactly what the precondition prevents. The next tick reads it and converges.
func TestCreateOrUpdateContentReadGrantLeavesAReplacedBindingAlone(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	g := testGrant("demo-content")

	if _, err := cs.RbacV1().RoleBindings(g.Namespace).Create(ctx, foreignBinding(g), metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed the foreign binding: %v", err)
	}
	before := len(cs.Actions())

	cs.PrependReactor("delete", "rolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			rbacv1.Resource("rolebindings"), g.Name, errors.New("precondition failed"))
	})

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("a replaced binding must not fail the reconcile: %v", err)
	}

	for _, a := range cs.Actions()[before:] {
		if a.GetResource().Resource == "rolebindings" && a.GetVerb() == "create" {
			t.Error("want no create after a failed delete precondition")
		}
	}
}

// TestCreateOrUpdateContentReadGrantReportsATerminatingBinding asserts a
// binding a finalizer holds is reported, not retried silently.
//
// A delete of such an object is accepted, but the object stays. Every create
// then fails with AlreadyExists. Treating that as success repeats the delete
// and the create on every tick and records nothing, so the grant never
// converges and nobody is told why.
func TestCreateOrUpdateContentReadGrantReportsATerminatingBinding(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	g := testGrant("demo-content")

	terminating := foreignBinding(g)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"example.com/protect"}
	if _, err := cs.RbacV1().RoleBindings(g.Namespace).Create(ctx, terminating, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed the terminating binding: %v", err)
	}
	before := len(cs.Actions())

	// The delete is accepted, and the finalizer keeps the object in place.
	cs.PrependReactor("delete", "rolebindings", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})

	err := c.CreateOrUpdateContentReadGrant(ctx, g)
	if err == nil {
		t.Fatal("want an error naming the terminating binding, got nil")
	}
	if !strings.Contains(err.Error(), "terminating") {
		t.Errorf("error = %v, want it to name the terminating binding", err)
	}

	for _, a := range cs.Actions()[before:] {
		if a.GetResource().Resource == "rolebindings" && a.GetVerb() == "create" {
			t.Error("want no create while the old binding is terminating")
		}
	}
}

// TestCreateOrUpdateContentReadGrantRestoresWidenedRule asserts the write
// converges a rule an administrator widened by hand. The grant must never keep
// a broader permission than the one it declares.
func TestCreateOrUpdateContentReadGrantRestoresWidenedRule(t *testing.T) {
	c := &ClientsetClient{clientset: fake.NewSimpleClientset()}
	ctx := context.Background()
	g := testGrant("demo-content")

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("create grant: %v", err)
	}

	role, err := c.clientset.RbacV1().Roles(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get role: %v", err)
	}
	role.Rules[0].ResourceNames = nil
	role.Rules[0].Verbs = []string{"get", "list", "watch"}
	if _, err := c.clientset.RbacV1().Roles(g.Namespace).Update(ctx, role, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("widen rule: %v", err)
	}

	if err := c.CreateOrUpdateContentReadGrant(ctx, g); err != nil {
		t.Fatalf("reconcile grant: %v", err)
	}

	role, err = c.clientset.RbacV1().Roles(g.Namespace).Get(ctx, g.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get role after reconcile: %v", err)
	}
	rule := role.Rules[0]
	if len(rule.ResourceNames) != 1 || rule.ResourceNames[0] != "demo-content" {
		t.Errorf("resourceNames = %v, want the name-scoped list restored", rule.ResourceNames)
	}
	if len(rule.Verbs) != 1 || rule.Verbs[0] != "get" {
		t.Errorf("verbs = %v, want [get] restored", rule.Verbs)
	}
}
