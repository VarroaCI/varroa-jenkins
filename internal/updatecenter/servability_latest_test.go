package updatecenter

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// fixedAvailability answers every lookup with one fixed availability.
type fixedAvailability struct{ a ucmeta.Availability }

func (f fixedAvailability) ResolveAvailable(_ context.Context, _, _ string) ucmeta.Availability {
	return f.a
}

func TestServability_LatestCarriesResolvedVersion(t *testing.T) {
	res := fixedAvailability{a: ucmeta.Availability{Outcome: ucmeta.Resolved, Via: ucmeta.ViaMetadata, SHA256: "x", Version: "4.0"}}
	got := servabilityFor(context.Background(), res, servabilityPlugin{Name: "git", Version: "latest"})
	if !got.Servable || got.Via != "metadata" || got.ResolvedVersion != "4.0" {
		t.Fatalf("got %+v, want servable via metadata with resolvedVersion 4.0", got)
	}
}

func TestServability_ConcreteVersionOmitsResolvedVersion(t *testing.T) {
	res := fixedAvailability{a: ucmeta.Availability{Outcome: ucmeta.Resolved, Via: ucmeta.ViaArchive, SHA256: "x", Version: "5.7.0"}}
	got := servabilityFor(context.Background(), res, servabilityPlugin{Name: "git", Version: "5.7.0"})
	if !got.Servable || got.ResolvedVersion != "" {
		t.Fatalf("got %+v, want servable with no resolvedVersion", got)
	}
}

func TestServability_LatestUnlistedNotListed(t *testing.T) {
	res := fixedAvailability{a: ucmeta.Availability{Outcome: ucmeta.NotListed}}
	got := servabilityFor(context.Background(), res, servabilityPlugin{Name: "ghost", Version: "latest"})
	if got.Servable || got.Reason != servabilityReasonNotListed {
		t.Fatalf("got %+v, want not-listed", got)
	}
}
