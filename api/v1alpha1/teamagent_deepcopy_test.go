package v1alpha1

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fullTeamAgent returns a TeamAgent that sets every pointer, slice and map.
func fullTeamAgent() *TeamAgent {
	enabled := true
	session, day := int64(1000), int64(5000)
	debounce := metav1.Duration{Duration: 5 * time.Minute}
	return &TeamAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-a", Namespace: "ns-a", Labels: map[string]string{"k": "v"}},
		Spec: TeamAgentSpec{
			TeamRef:          TeamAgentTeamRef{Name: "team-a"},
			Enabled:          &enabled,
			Provider:         "p",
			Model:            "m",
			Instructions:     &TeamAgentInstructions{Source: "repo", Path: "a/b.md"},
			Triggers:         []TeamAgentTrigger{{Type: "run.failed", Debounce: &debounce}, {Type: "manual"}},
			UnattendedWrites: []string{"run_pipeline"},
			Outputs: TeamAgentOutputs{
				Notes:   true,
				Issues:  &TeamAgentIssueOutput{Repo: "https://git.example.org/o/r", Forge: "gitea", Labels: []string{"bug"}},
				Webhook: true,
			},
			Budgets:     &TeamAgentBudgets{Session: &session, Day: &day},
			DataSharing: "logs",
		},
		Status: TeamAgentStatus{
			Conditions:         []metav1.Condition{{Type: "Paused", Status: metav1.ConditionFalse, Reason: "Resolved"}},
			InstructionsSHA:    "abc",
			LastSession:        &TeamAgentLastSession{ID: "s1", Trigger: "manual", State: "done", At: metav1.NewTime(time.Unix(1700000000, 0))},
			SpendToday:         7,
			ObservedGeneration: 2,
		},
	}
}

// TestTeamAgentDeepCopy checks that each generated copy equals its source,
// and that a change to the copy leaves the source unchanged.
func TestTeamAgentDeepCopy(t *testing.T) {
	t.Run("TeamAgent", func(t *testing.T) {
		src := fullTeamAgent()
		cp := src.DeepCopy()
		if !reflect.DeepEqual(src, cp) {
			t.Fatal("copy differs from source")
		}
		*cp.Spec.Enabled = false
		cp.Spec.Triggers[0].Type = "changed"
		*cp.Spec.Triggers[0].Debounce = metav1.Duration{}
		cp.Spec.UnattendedWrites[0] = "changed"
		cp.Spec.Outputs.Issues.Labels[0] = "changed"
		*cp.Spec.Budgets.Session = 1
		cp.Status.Conditions[0].Reason = "changed"
		cp.Status.LastSession.ID = "changed"
		cp.Labels["k"] = "changed"
		if !reflect.DeepEqual(src, fullTeamAgent()) {
			t.Fatal("mutating the copy changed the source")
		}
		if _, ok := src.DeepCopyObject().(*TeamAgent); !ok {
			t.Fatal("DeepCopyObject returns the wrong type")
		}
	})
	t.Run("TeamAgentList", func(t *testing.T) {
		src := &TeamAgentList{Items: []TeamAgent{*fullTeamAgent()}}
		cp := src.DeepCopy()
		if !reflect.DeepEqual(src, cp) {
			t.Fatal("copy differs from source")
		}
		cp.Items[0].Spec.TeamRef.Name = "changed"
		if src.Items[0].Spec.TeamRef.Name != "team-a" {
			t.Fatal("mutating the copy changed the source")
		}
	})
	t.Run("TeamAgentSpec", func(t *testing.T) {
		src := fullTeamAgent().Spec
		cp := src.DeepCopy()
		if !reflect.DeepEqual(&src, cp) {
			t.Fatal("copy differs from source")
		}
		cp.Instructions.Path = "changed"
		if src.Instructions.Path != "a/b.md" {
			t.Fatal("mutating the copy changed the source")
		}
	})
	t.Run("TeamAgentStatus", func(t *testing.T) {
		src := fullTeamAgent().Status
		cp := src.DeepCopy()
		if !reflect.DeepEqual(&src, cp) {
			t.Fatal("copy differs from source")
		}
		cp.LastSession.State = "changed"
		if src.LastSession.State != "done" {
			t.Fatal("mutating the copy changed the source")
		}
	})
	t.Run("TeamAgentInstructions", func(t *testing.T) {
		src := fullTeamAgent().Spec.Instructions
		if cp := src.DeepCopy(); !reflect.DeepEqual(src, cp) {
			t.Fatal("copy differs from source")
		}
	})
	t.Run("TeamAgentTrigger", func(t *testing.T) {
		src := fullTeamAgent().Spec.Triggers[0]
		cp := src.DeepCopy()
		if !reflect.DeepEqual(&src, cp) {
			t.Fatal("copy differs from source")
		}
		cp.Debounce.Duration = 0
		if src.Debounce.Duration != 5*time.Minute {
			t.Fatal("mutating the copy changed the source")
		}
	})
	t.Run("TeamAgentOutputs", func(t *testing.T) {
		src := fullTeamAgent().Spec.Outputs
		cp := src.DeepCopy()
		if !reflect.DeepEqual(&src, cp) {
			t.Fatal("copy differs from source")
		}
		cp.Issues.Labels[0] = "changed"
		if src.Issues.Labels[0] != "bug" {
			t.Fatal("mutating the copy changed the source")
		}
	})
	t.Run("TeamAgentIssueOutput", func(t *testing.T) {
		src := fullTeamAgent().Spec.Outputs.Issues
		cp := src.DeepCopy()
		if !reflect.DeepEqual(src, cp) {
			t.Fatal("copy differs from source")
		}
		cp.Labels[0] = "changed"
		if src.Labels[0] != "bug" {
			t.Fatal("mutating the copy changed the source")
		}
	})
	t.Run("TeamAgentBudgets", func(t *testing.T) {
		src := fullTeamAgent().Spec.Budgets
		cp := src.DeepCopy()
		if !reflect.DeepEqual(src, cp) {
			t.Fatal("copy differs from source")
		}
		*cp.Day = 1
		if *src.Day != 5000 {
			t.Fatal("mutating the copy changed the source")
		}
	})
	t.Run("TeamAgentLastSession", func(t *testing.T) {
		src := fullTeamAgent().Status.LastSession
		cp := src.DeepCopy()
		if !reflect.DeepEqual(src, cp) {
			t.Fatal("copy differs from source")
		}
		cp.Trigger = "changed"
		if src.Trigger != "manual" {
			t.Fatal("mutating the copy changed the source")
		}
	})
}
