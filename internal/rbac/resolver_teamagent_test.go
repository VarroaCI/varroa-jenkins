package rbac

import (
	"reflect"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// TestResolver_AgentSubjectMatchesWithEmailClaim checks that a Team agent
// matches its binding through the subject even when the configured user claim
// is email and the email is empty.
func TestResolver_AgentSubjectMatchesWithEmailClaim(t *testing.T) {
	claims := &auth.Claims{Subject: "agent:ns-a/team-a/agent-a", PreferredUsername: "agent:ns-a/team-a/agent-a"}
	got := userSubjectValues(claims, []string{"email"})
	if want := []string{"agent:ns-a/team-a/agent-a"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("userSubjectValues = %v, want %v", got, want)
	}
}

// TestResolver_HumanAgentPrefixedUsernameDropped checks that a human identity
// cannot match an agent binding through a value with the agent prefix.
func TestResolver_HumanAgentPrefixedUsernameDropped(t *testing.T) {
	claims := &auth.Claims{Subject: "alice", PreferredUsername: "agent:ns-a/team-a/agent-a", Email: "alice@example.org"}
	got := userSubjectValues(claims, []string{"preferred_username", "email"})
	if want := []string{"alice@example.org"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("userSubjectValues = %v, want %v", got, want)
	}
}
