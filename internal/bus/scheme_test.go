package bus

import (
	"strings"
	"testing"
	"time"
)

func TestBusScheme(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"mite in", MiteInSubject("core", "team-a", "foo"), "mite.core.team-a.foo.in"},
		{"mite out", MiteOutSubject("core", "team-a", "foo"), "mite.core.team-a.foo.out"},
		{"mite content", MiteContentSubject("core", "team-a", "foo"), "mite.core.team-a.foo.content"},
		{"brood", BroodSubject("core"), "events.brood.core"},
		{"brood wildcard", BroodWildcard, "events.brood.>"},
		{"activity", ActivitySubject("core", "team-a", "foo"), "activity.core.team-a.foo"},
		{"activity global", ActivityGlobal("core"), "activity.core._global"},
		{"activity wildcard", ActivityWildcard, "activity.>"},
		{"op reconcile", OperatorReconcileSubject("core"), "operator.core.reconcile"},
		{"op nudge", OperatorNudgeSubject("core"), "operator.core.nudge"},
		{"op wake", OperatorWakeSubject("core"), "operator.core.wake"},
		{"op hibernate", OperatorHibernateSubject("core"), "operator.core.hibernate"},
		{"op approverestart", OperatorApproveSubject("core"), "operator.core.approverestart"},
		{"op reprovision", OperatorReprovisionSubject("core"), "operator.core.reprovision"},
		{"op approvedeletion", OperatorApproveDeletionSubject("core"), "operator.core.approvedeletion"},
		{"op crud list", OperatorControllersSubject("core", "list"), "operator.core.controllers.list"},
		{"op crud get", OperatorControllersSubject("core", "get"), "operator.core.controllers.get"},
		{"op crud create", OperatorControllersSubject("core", "create"), "operator.core.controllers.create"},
		{"op crud update", OperatorControllersSubject("dev-cluster", "update"), "operator.dev-cluster.controllers.update"},
		{"op crud delete", OperatorControllersSubject("core", "delete"), "operator.core.controllers.delete"},
		{"op crud deletepod", OperatorControllersSubject("dev-cluster", "deletepod"), "operator.dev-cluster.controllers.deletepod"},
		{"op broodops create", OperatorBroodOpsSubject("dev-cluster", "create"), "operator.dev-cluster.broodops.create"},
		{"op broodops get", OperatorBroodOpsSubject("core", "get"), "operator.core.broodops.get"},
		{"kv snapshot", SnapshotKey("core", "team-a", "foo"), "core/team-a/foo"},
		{"kv obs", ObservabilityKey("core", "team-a", "foo"), "obs/core/team-a/foo"},
		{"kv presence", PresenceKey("core", "team-a", "foo"), "core/team-a/foo"},
		{"kv desired", DesiredKey("core", "team-a", "foo"), "core/team-a/foo"},
		{"varroa stream", StreamConfig("varroa").Subjects[0], "mite.*.*.*.out"},
		{"activity stream", ActivityStreamConfig("varroa_activity", time.Hour, 1, 1).Subjects[0], "activity.>"},
		{"webhook", WebhookSubject("core", "team-a", "foo"), "webhook.core.team-a.foo"},
		{"wake", WakeSubject("core", "team-a", "foo"), "wake.core.team-a.foo"},
		{"wake wildcard", WakeSubjectWildcard("dev-cluster"), "wake.dev-cluster.>"},
		{"webhook result", WebhookResultSubject("core", "team-a", "foo"), "whreply.core.team-a.foo"},
		{"webhook result wildcard", WebhookResultWildcard("core"), "whreply.core.>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

// TestWebhookResultDisjointFromStream guards the invariant that replay-result
// subjects are never captured by the varroa_webhooks stream ("webhook.>").
func TestWebhookResultDisjointFromStream(t *testing.T) {
	result := WebhookResultSubject("core", "ns", "ctrl")
	if strings.HasPrefix(result, "webhook.") {
		t.Fatalf("result subject %q would be captured by the webhook.> stream", result)
	}
}

func TestParseWakeSubject(t *testing.T) {
	cases := []struct {
		name, subject   string
		wantNS, wantCtl string
		wantOK          bool
	}{
		{"valid", "wake.dev-cluster.team-a.foo", "team-a", "foo", true},
		{"roundtrip", WakeSubject("core", "ns1", "ctrl1"), "ns1", "ctrl1", true},
		{"too few tokens", "wake.core.ns", "", "", false},
		{"too many tokens", "wake.core.ns.ctrl.extra", "", "", false},
		{"wrong prefix", "sleep.core.ns.ctrl", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, ctrl, ok := ParseWakeSubject(tc.subject)
			if ok != tc.wantOK || ns != tc.wantNS || ctrl != tc.wantCtl {
				t.Fatalf("ParseWakeSubject(%q) = (%q,%q,%v), want (%q,%q,%v)",
					tc.subject, ns, ctrl, ok, tc.wantNS, tc.wantCtl, tc.wantOK)
			}
		})
	}
}

func TestClusterFromEnv(t *testing.T) {
	t.Run("unset returns core", func(t *testing.T) {
		t.Setenv("VARROA_CLUSTER_NAME", "")
		got, err := ClusterFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "core" {
			t.Fatalf("got %q, want %q", got, "core")
		}
	})

	t.Run("valid label", func(t *testing.T) {
		t.Setenv("VARROA_CLUSTER_NAME", "dev-cluster")
		got, err := ClusterFromEnv()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "dev-cluster" {
			t.Fatalf("got %q, want %q", got, "dev-cluster")
		}
	})

	t.Run("invalid label with underscore", func(t *testing.T) {
		t.Setenv("VARROA_CLUSTER_NAME", "Prod_East")
		_, err := ClusterFromEnv()
		if err == nil {
			t.Fatal("expected error for invalid label")
		}
	})

	t.Run("too long", func(t *testing.T) {
		t.Setenv("VARROA_CLUSTER_NAME", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") // 70 chars
		_, err := ClusterFromEnv()
		if err == nil {
			t.Fatal("expected error for too-long label")
		}
	})

	t.Run("starts with hyphen", func(t *testing.T) {
		t.Setenv("VARROA_CLUSTER_NAME", "-bad")
		_, err := ClusterFromEnv()
		if err == nil {
			t.Fatal("expected error for label starting with hyphen")
		}
	})
}

func TestClusterIsInstanceScoped(t *testing.T) {
	// Two different cluster values produce different outputs — no shared state.
	coreIn := MiteInSubject("core", "ns1", "ctrl1")
	devIn := MiteInSubject("dev-cluster", "ns1", "ctrl1")
	if coreIn == devIn {
		t.Fatal("expected different subjects for different clusters")
	}
	if coreIn != "mite.core.ns1.ctrl1.in" {
		t.Fatalf("core subject mismatch: %q", coreIn)
	}
	if devIn != "mite.dev-cluster.ns1.ctrl1.in" {
		t.Fatalf("dev subject mismatch: %q", devIn)
	}

	// KV keys also scoped.
	coreKey := SnapshotKey("core", "ns1", "ctrl1")
	devKey := SnapshotKey("dev-cluster", "ns1", "ctrl1")
	if coreKey == devKey {
		t.Fatal("expected different KV keys for different clusters")
	}
	if coreKey != "core/ns1/ctrl1" {
		t.Fatalf("core key mismatch: %q", coreKey)
	}
	if devKey != "dev-cluster/ns1/ctrl1" {
		t.Fatalf("dev key mismatch: %q", devKey)
	}
}
