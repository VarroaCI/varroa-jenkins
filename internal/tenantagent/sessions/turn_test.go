package sessions

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
)

// TestTurnText_Layout proves that the first turn carries the trigger, the item,
// the build, the run, and the instructions block with its delimiters.
func TestTurnText_Layout(t *testing.T) {
	sess := agent.Session{
		Trigger: "run.failed", Cluster: "core", Tenant: "obj",
		Context: map[string]any{"run": "run-1", "build": int64(7)},
	}
	e := activity.Event{ItemPath: "obj/build", Message: "deploy failed"}
	got := TurnText(sess, e, ".varroa/tenant-agent.md", "rev-1", "be brief", true)
	for _, want := range []string{
		"Trigger: run.failed on core, tenant obj.\n",
		"Item: obj/build   Build: 7   Run: run-1\n",
		"Event: deploy failed\n",
		"--- Tenant instructions (.varroa/tenant-agent.md at rev-1) ---\nbe brief\n--- end of Tenant instructions ---\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("first turn lacks %q:\n%s", want, got)
		}
	}
	if without := TurnText(sess, e, ".varroa/tenant-agent.md", "rev-1", "", false); strings.Contains(without, "Tenant instructions") {
		t.Errorf("an empty body still renders the instructions block")
	}
}
