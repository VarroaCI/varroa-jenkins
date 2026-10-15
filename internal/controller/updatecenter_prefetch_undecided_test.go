package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testPrefetchUndecidedWaits(t *testing.T) {
	client := setupPrefetchClient(t, true, "foo@1.0")
	fake := &prefetchTestUC{unserv: map[string]string{"foo@1.0": "sources-degraded"}}
	ts := fake.serve(t)

	status := reconcilePrefetchOnce(t, client, ts)
	if len(fake.posted) != 0 {
		t.Fatalf("prefetch posted an undecided plugin: %v", fake.posted)
	}
	c := prefetchCond(t, status)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != reasonServabilityUnavailable {
		t.Fatalf("PluginsPrefetched = %+v, want Unknown/ServabilityUnavailable", c)
	}
}
