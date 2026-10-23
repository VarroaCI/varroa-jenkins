package accept

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

const release = "varroa-system"

func agent(ns, name, tenant string, created time.Time) v1alpha1.TenantAgent {
	return v1alpha1.TenantAgent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       v1alpha1.TenantAgentSpec{TenantRef: v1alpha1.TenantAgentTenantRef{Name: tenant}},
	}
}

func key(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

func TestDecide(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	tenants := map[string]*v1alpha1.Tenant{
		"team-a": {Spec: v1alpha1.TenantSpec{Namespaces: []string{"team-a-ns"}}},
		"team-b": {Spec: v1alpha1.TenantSpec{Namespaces: []string{"team-b-ns"}}},
	}
	cases := []struct {
		name    string
		in      []v1alpha1.TenantAgent
		tenants map[string]*v1alpha1.Tenant
		want    map[types.NamespacedName]Verdict
	}{
		{
			name:    "owned namespace accepted",
			in:      []v1alpha1.TenantAgent{agent("team-a-ns", "a", "team-a", t0)},
			tenants: tenants,
			want:    map[types.NamespacedName]Verdict{key("team-a-ns", "a"): {Accepted: true}},
		},
		{
			name:    "foreign namespace refused",
			in:      []v1alpha1.TenantAgent{agent("team-b-ns", "a", "team-a", t0)},
			tenants: tenants,
			want:    map[types.NamespacedName]Verdict{key("team-b-ns", "a"): {Reason: ReasonNamespaceNotOwned}},
		},
		{
			name:    "member Tenant needs the release namespace when absent from tenants",
			in:      []v1alpha1.TenantAgent{agent(release, "a", "team-x", t0), agent("other", "b", "team-x", t0)},
			tenants: tenants,
			want: map[types.NamespacedName]Verdict{
				key(release, "a"): {Accepted: true},
				key("other", "b"): {Reason: ReasonNamespaceNotOwned},
			},
		},
		{
			name:    "oldest wins, the newer one is a duplicate",
			in:      []v1alpha1.TenantAgent{agent("team-a-ns", "new", "team-a", t1), agent("team-a-ns", "old", "team-a", t0)},
			tenants: tenants,
			want: map[types.NamespacedName]Verdict{
				key("team-a-ns", "old"): {Accepted: true},
				key("team-a-ns", "new"): {Reason: ReasonDuplicate},
			},
		},
		{
			name:    "equal age, lower namespace/name wins",
			in:      []v1alpha1.TenantAgent{agent("team-a-ns", "z", "team-a", t0), agent("team-a-ns", "m", "team-a", t0)},
			tenants: tenants,
			want: map[types.NamespacedName]Verdict{
				key("team-a-ns", "m"): {Accepted: true},
				key("team-a-ns", "z"): {Reason: ReasonDuplicate},
			},
		},
		{
			name: "disabled object stays accepted",
			in: func() []v1alpha1.TenantAgent {
				off := false
				ta := agent("team-a-ns", "a", "team-a", t0)
				ta.Spec.Enabled = &off
				return []v1alpha1.TenantAgent{ta}
			}(),
			tenants: tenants,
			want:    map[types.NamespacedName]Verdict{key("team-a-ns", "a"): {Accepted: true}},
		},
		{
			name:    "one agent per tenant: a second Tenant is independent",
			in:      []v1alpha1.TenantAgent{agent("team-a-ns", "a", "team-a", t0), agent("team-b-ns", "b", "team-b", t1)},
			tenants: tenants,
			want: map[types.NamespacedName]Verdict{
				key("team-a-ns", "a"): {Accepted: true},
				key("team-b-ns", "b"): {Accepted: true},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(Input{TenantAgents: c.in, Tenants: c.tenants, ReleaseNamespace: release})
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Decide() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPrincipal(t *testing.T) {
	ta := agent("ns-a", "agent-1", "team-a", time.Time{})
	if got, want := Principal(&ta), "agent:ns-a/team-a/agent-1"; got != want {
		t.Fatalf("Principal() = %q, want %q", got, want)
	}
	if !IsPrincipal("agent:x/y/z") || IsPrincipal("alice") {
		t.Fatal("IsPrincipal misclassifies a subject")
	}
	if !Enabled(&ta) {
		t.Fatal("a nil enabled must read as on")
	}
}

func TestOutputToolNames(t *testing.T) {
	outputs := v1alpha1.TenantAgentOutputs{Notes: true, Issues: &v1alpha1.TenantAgentIssueOutput{}, Webhook: true}
	kinds := OutputKinds(outputs)
	if want := []string{"note", "issue", "webhook"}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("OutputKinds() = %v, want %v", kinds, want)
	}
	if got, want := OutputToolNames(kinds), []string{"post_note", "open_issue", "send_webhook"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("OutputToolNames() = %v, want %v", got, want)
	}
	if got := OutputToolNames([]string{"bogus"}); len(got) != 0 {
		t.Fatalf("an unknown kind must be dropped, got %v", got)
	}
}

// TestAcceptImportsOnlyAPI keeps the package a leaf. Every repo package it
// depends on, directly or not, must be api/v1alpha1.
func TestAcceptImportsOnlyAPI(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	const module = "github.com/varroaci/varroa-jenkins/"
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, module) && dep != module+"api/v1alpha1" && dep != module+"internal/tenantagent/accept" {
			t.Errorf("accept depends on repo package %s", dep)
		}
	}
}
