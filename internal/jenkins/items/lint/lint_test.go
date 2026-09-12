package lint

import (
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/jenkins/items"
)

// TestRun_AppliesEveryRuleToEveryItem proves the runner returns one finding
// per offence, each naming its rule id and its item path, and that a
// clean item produces none.
func TestRun_AppliesEveryRuleToEveryItem(t *testing.T) {
	manifest := &items.Manifest{
		Items: []items.Item{
			{
				Kind: "folder",
				Name: "tenant",
				Items: []items.Item{
					{
						Kind: "pipeline",
						Name: "offender",
						Definition: &items.PipelineDefinition{
							Script: "echo hi",
						},
						Triggers: []items.Trigger{
							{PollSCM: &items.PollSCMTrigger{}},
						},
					},
					{
						Kind: "pipeline",
						Name: "clean",
						Definition: &items.PipelineDefinition{
							Script: "echo hi",
						},
					},
				},
			},
		},
	}

	findings := Run(manifest, []Rule{NoPollSCMRule{}})
	if len(findings) != 1 {
		t.Fatalf("expected exactly one finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Rule != "no-poll-scm" {
		t.Errorf("expected rule id no-poll-scm, got %q", f.Rule)
	}
	if f.Path != "tenant/offender" {
		t.Errorf("expected path tenant/offender, got %q", f.Path)
	}
}

func TestRun_TwoOffendingItemsProduceTwoFindings(t *testing.T) {
	manifest := &items.Manifest{
		Items: []items.Item{
			{
				Kind:     "pipeline",
				Name:     "a",
				Triggers: []items.Trigger{{PollSCM: &items.PollSCMTrigger{}}},
			},
			{
				Kind:     "pipeline",
				Name:     "b",
				Triggers: []items.Trigger{{PollSCM: &items.PollSCMTrigger{}}},
			},
		},
	}

	findings := Run(manifest, []Rule{NoPollSCMRule{}})
	if len(findings) != 2 {
		t.Fatalf("expected two findings, one per offending item, got %d: %+v", len(findings), findings)
	}
}
