package updatecenter

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

func (f fixedAvailability) ResolveLatestForCore(_ context.Context, _, _ string) ucmeta.Availability {
	return f.a
}

func (f *inflightResolver) ResolveLatestForCore(ctx context.Context, name, _ string) ucmeta.Availability {
	return f.ResolveAvailable(ctx, name, "latest")
}

// coreRecordingResolver records which resolve method a lookup used, and the core.
type coreRecordingResolver struct {
	availableCalls []string
	coreCalls      []string
	answer         ucmeta.Availability
}

func (f *coreRecordingResolver) ResolveAvailable(_ context.Context, name, version string) ucmeta.Availability {
	f.availableCalls = append(f.availableCalls, name+"@"+version)
	return f.answer
}

func (f *coreRecordingResolver) ResolveLatestForCore(_ context.Context, name, core string) ucmeta.Availability {
	f.coreCalls = append(f.coreCalls, name+"@"+core)
	return f.answer
}

func TestServability_LatestWithCoreUsesCompatibleResolve(t *testing.T) {
	res := &coreRecordingResolver{answer: ucmeta.Availability{Outcome: ucmeta.Resolved, Via: ucmeta.ViaMetadata, SHA256: "x", Version: "1.9"}}
	got := servabilityFor(context.Background(), res, servabilityPlugin{Name: "foo", Version: "latest", Core: "2.555.3"})
	if !got.Servable || got.ResolvedVersion != "1.9" {
		t.Fatalf("got %+v, want servable with resolvedVersion 1.9", got)
	}
	if len(res.coreCalls) != 1 || res.coreCalls[0] != "foo@2.555.3" {
		t.Fatalf("coreCalls = %v, want [foo@2.555.3]", res.coreCalls)
	}
	if len(res.availableCalls) != 0 {
		t.Fatalf("availableCalls = %v, want none", res.availableCalls)
	}
}

func TestServability_LatestWithCoreNoCompatibleNotListed(t *testing.T) {
	res := &coreRecordingResolver{answer: ucmeta.Availability{Outcome: ucmeta.NotListed}}
	got := servabilityFor(context.Background(), res, servabilityPlugin{Name: "foo", Version: "latest", Core: "2.555.3"})
	if got.Servable || got.Reason != servabilityReasonNotListed {
		t.Fatalf("got %+v, want not-listed", got)
	}
	if got.ResolvedVersion != "" {
		t.Fatalf("resolvedVersion = %q, want empty", got.ResolvedVersion)
	}
}

func TestServability_PinnedVersionIgnoresCore(t *testing.T) {
	res := &coreRecordingResolver{answer: ucmeta.Availability{Outcome: ucmeta.Resolved, Via: ucmeta.ViaMetadata, SHA256: "x", Version: "1.5"}}
	got := servabilityFor(context.Background(), res, servabilityPlugin{Name: "foo", Version: "1.5", Core: "2.555.3"})
	if !got.Servable || got.ResolvedVersion != "" {
		t.Fatalf("got %+v, want servable with no resolvedVersion", got)
	}
	if len(res.coreCalls) != 0 || len(res.availableCalls) != 1 || res.availableCalls[0] != "foo@1.5" {
		t.Fatalf("coreCalls = %v, availableCalls = %v; want the pinned path only", res.coreCalls, res.availableCalls)
	}
}
