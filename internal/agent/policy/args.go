package policy

import (
	"encoding/json"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// Args holds the top-level string arguments that name a target.
type Args struct {
	Run       string
	Tenant    string
	Job       string
	Cluster   string
	Namespace string
	Name      string
}

// ParseArgs reads the top-level string fields of a tool call's arguments.
// Bad JSON, or a named field that is not a string, gives the zero Args.
func ParseArgs(args json.RawMessage) Args {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return Args{}
	}
	var out Args
	targets := map[string]*string{
		"run": &out.Run, "tenant": &out.Tenant, "job": &out.Job,
		"cluster": &out.Cluster, "namespace": &out.Namespace, "name": &out.Name,
	}
	for key, raw := range fields {
		dst, known := targets[key]
		if !known {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return Args{}
		}
		*dst = s
	}
	return out
}

// Target returns the kind and reference of the object a tool call acts on.
// It reads no default cluster.
func Target(name string, args json.RawMessage) (agent.TargetKind, string) {
	return TargetIn(name, args, "")
}

// TargetIn returns the kind and reference of the object a tool call acts on.
// The first rule that matches wins: a run argument names a run; a tenant and
// a job name a job; a controller tool with a name names a controller, and
// it uses defaultCluster when the call names no cluster. Every other call
// names no target.
func TargetIn(name string, args json.RawMessage, defaultCluster string) (agent.TargetKind, string) {
	a := ParseArgs(args)
	switch {
	case a.Run != "":
		return agent.TargetRun, a.Run
	case a.Tenant != "" && a.Job != "":
		return agent.TargetJob, a.Tenant + "/" + a.Job
	case strings.HasSuffix(name, "_controller") && a.Name != "":
		cluster := a.Cluster
		if cluster == "" {
			cluster = defaultCluster
		}
		return agent.TargetController, cluster + "/" + a.Namespace + "/" + a.Name
	}
	return agent.TargetNone, ""
}
