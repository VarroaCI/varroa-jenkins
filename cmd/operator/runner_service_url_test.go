package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
)

// TestRunnerServiceURL_MatchesAPIJenkinsServiceURL pins
// runners.CellServiceURL — the liveness probe's own formula (design D10)
// — and api.BuildJenkinsServiceURL — the agent door's formula (design D7,
// D8) — to one expected string for the same Controller. The two packages
// cannot import each other without an import cycle, so each carries its
// own copy of the formula; this is the one place both are importable, and
// it is what catches the two copies drifting apart.
func TestRunnerServiceURL_MatchesAPIJenkinsServiceURL(t *testing.T) {
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-a",
			Namespace: "team-ns",
			UID:       "0123456789abcdef",
		},
	}
	const want = "http://cell-a-01234567-svc.team-ns.svc.cluster.local:8080"

	if got := runners.CellServiceURL(cr); got != want {
		t.Fatalf("runners.CellServiceURL = %q, want %q", got, want)
	}
	if got := api.BuildJenkinsServiceURL(cr); got != want {
		t.Fatalf("api.BuildJenkinsServiceURL = %q, want %q", got, want)
	}
}
