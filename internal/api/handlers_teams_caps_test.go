package api

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

func TestValidateTeamPipelines_RejectsBadLimit(t *testing.T) {
	t.Parallel()

	ok := []v1alpha1.TeamConcurrencyLimit{{Name: "deploys", Jobs: []string{"deploy/*"}, Max: 1}}
	base := func(limits []v1alpha1.TeamConcurrencyLimit) *teamPipelinesEntry {
		return &teamPipelinesEntry{Profile: "p", AgentNamespace: "jenkins", ConcurrencyLimits: limits}
	}
	if msg := validateTeamPipelines(base(ok), []string{"jenkins"}); msg != "" {
		t.Fatalf("a valid limit was refused: %s", msg)
	}
	cases := map[string][]v1alpha1.TeamConcurrencyLimit{
		"empty jobs":     {{Name: "a", Jobs: nil, Max: 1}},
		"leading slash":  {{Name: "a", Jobs: []string{"/x"}, Max: 1}},
		"max zero":       {{Name: "a", Jobs: []string{"*"}, Max: 0}},
		"duplicate name": {{Name: "a", Jobs: []string{"*"}, Max: 1}, {Name: "a", Jobs: []string{"*"}, Max: 2}},
		"bad name":       {{Name: "Not_Valid", Jobs: []string{"*"}, Max: 1}},
	}
	for label, limits := range cases {
		msg := validateTeamPipelines(base(limits), []string{"jenkins"})
		if !strings.HasPrefix(msg, "pipelines.concurrencyLimits[") {
			t.Errorf("%s: message = %q, want a concurrencyLimits refusal", label, msg)
		}
	}
}
