package policy

import (
	"encoding/json"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

func boolp(b bool) *bool { return &b }

func TestClassFromHints(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		readOnly   *bool
		openWorld  *bool
		wantClass  agent.ToolClass
		wantHidden bool
	}{
		{"nil hints are write and visible", nil, nil, agent.ClassWrite, false},
		{"read-only true is read", boolp(true), nil, agent.ClassRead, false},
		{"read-only false is write", boolp(false), nil, agent.ClassWrite, false},
		{"open-world true hides", nil, boolp(true), agent.ClassWrite, true},
		{"open-world false shows", nil, boolp(false), agent.ClassWrite, false},
	}
	for _, c := range cases {
		class, hidden := ClassFromHints(c.readOnly, c.openWorld)
		if class != c.wantClass || hidden != c.wantHidden {
			t.Fatalf("%s: got (%s, %v), want (%s, %v)", c.name, class, hidden, c.wantClass, c.wantHidden)
		}
	}
}

func TestIsNeverUnattended(t *testing.T) {
	t.Parallel()

	del := agent.ToolDef{Name: "delete_controller", Class: agent.ClassWrite}
	if !IsNeverUnattended(del, nil) {
		t.Fatal("delete_controller with empty output must need an approval")
	}
	run := agent.ToolDef{Name: "run_pipeline", Class: agent.ClassWrite}
	if IsNeverUnattended(run, nil) {
		t.Fatal("run_pipeline is eligible and must not need an approval")
	}
	if !IsNeverUnattended(agent.ToolDef{Name: "replay_run", Class: agent.ClassWrite}, []string{"replay_run"}) {
		t.Fatal("replay_run is never unattended, whatever the output")
	}
	post := agent.ToolDef{Name: "post_note", Class: agent.ClassWrite}
	if IsNeverUnattended(post, []string{"post_note"}) {
		t.Fatal("a tool named in the output list must not need an approval")
	}
	read := agent.ToolDef{Name: "get_run", Class: agent.ClassRead}
	if IsNeverUnattended(read, nil) {
		t.Fatal("a read tool must not need an approval")
	}
}

func TestTarget(t *testing.T) {
	t.Parallel()

	kind, ref := Target("get_run", json.RawMessage(`{"run":"r-1"}`))
	if kind != agent.TargetRun || ref != "r-1" {
		t.Fatalf("run = (%s, %q)", kind, ref)
	}
	kind, ref = Target("run_pipeline", json.RawMessage(`{"team":"acme","job":"deploy"}`))
	if kind != agent.TargetJob || ref != "acme/deploy" {
		t.Fatalf("job = (%s, %q)", kind, ref)
	}
	kind, ref = Target("delete_controller", json.RawMessage(`{"cluster":"core","namespace":"jenkins","name":"c1"}`))
	if kind != agent.TargetController || ref != "core/jenkins/c1" {
		t.Fatalf("controller = (%s, %q)", kind, ref)
	}
	kind, ref = Target("delete_controller", json.RawMessage(`{"namespace":"jenkins","name":"c1"}`))
	if kind != agent.TargetController || ref != "/jenkins/c1" {
		t.Fatalf("controller without cluster = (%s, %q)", kind, ref)
	}
	kind, ref = TargetIn("delete_controller", json.RawMessage(`{"namespace":"jenkins","name":"c1"}`), "edge")
	if kind != agent.TargetController || ref != "edge/jenkins/c1" {
		t.Fatalf("controller with default cluster = (%s, %q)", kind, ref)
	}
	kind, ref = Target("list_controllers", json.RawMessage(`{}`))
	if kind != agent.TargetNone || ref != "" {
		t.Fatalf("none = (%s, %q)", kind, ref)
	}
	kind, ref = Target("get_run", json.RawMessage(`not json`))
	if kind != agent.TargetNone || ref != "" {
		t.Fatalf("bad JSON = (%s, %q)", kind, ref)
	}
	kind, _ = Target("get_run", json.RawMessage(`{"run":5}`))
	if kind != agent.TargetNone {
		t.Fatalf("non-string run gave target %s", kind)
	}
}

func TestRegistryListsAreDisjoint(t *testing.T) {
	t.Parallel()

	for _, name := range UnattendedEligible {
		for _, never := range NeverUnattended {
			if name == never {
				t.Fatalf("%s is both eligible and never unattended", name)
			}
		}
	}
}
