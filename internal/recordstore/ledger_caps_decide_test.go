package recordstore

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// capTestRun is a run in flight on the test tenant.
func capTestRun(job, cell string) inFlightRun {
	return inFlightRun{runID: uuid.Must(uuid.NewV7()), jobPath: "acme/" + job, cell: cell}
}

// capTestWaiter is a live tenant-limit waiter that waited since first.
func capTestWaiter(name string, first time.Time) capWaiter {
	return capWaiter{runID: uuid.Must(uuid.NewV7()), jobPath: "acme/app/build", cell: "cell-a",
		kind: capKindTenant, name: name, firstSeen: first}
}

var capTestLimit = []ConcurrencyLimit{{Name: "builds", Jobs: []string{"app/*"}, Max: 2}}

func TestDecideCaps_UnderLimitAllows(t *testing.T) {
	t.Parallel()

	req := ClaimRequest{Tenant: "acme", Cell: "cell-a", JobPath: "acme/app/build"}
	v := decideCaps("acme", req, uuid.Must(uuid.NewV7()), time.Now(), capTestLimit, 0,
		[]inFlightRun{capTestRun("app/test", "cell-a")}, nil)
	if v.Blocked {
		t.Fatalf("one of two builds running must allow, got %+v", v)
	}
}

func TestDecideCaps_AtLimitBlocksWithName(t *testing.T) {
	t.Parallel()

	req := ClaimRequest{Tenant: "acme", Cell: "cell-a", JobPath: "acme/app/build"}
	v := decideCaps("acme", req, uuid.Must(uuid.NewV7()), time.Now(), capTestLimit, 0,
		[]inFlightRun{capTestRun("app/test", "cell-a"), capTestRun("app/lint", "cell-a")}, nil)
	if !v.Blocked || v.Kind != capKindTenant || v.Name != "builds" {
		t.Fatalf("two builds running must block on builds, got %+v", v)
	}
	if !strings.Contains(v.Message, `"builds": 2 of 2 builds run`) {
		t.Fatalf("message = %q", v.Message)
	}
}

func TestDecideCaps_OlderWaiterOnSameLimitBlocks(t *testing.T) {
	t.Parallel()

	now := time.Now()
	req := ClaimRequest{Tenant: "acme", Cell: "cell-a", JobPath: "acme/app/build"}
	v := decideCaps("acme", req, uuid.Must(uuid.NewV7()), now, capTestLimit, 0,
		[]inFlightRun{capTestRun("app/test", "cell-a")},
		[]capWaiter{capTestWaiter("builds", now.Add(-time.Minute))})
	if !v.Blocked {
		t.Fatalf("an older waiter on the same limit must block the newer run, got %+v", v)
	}
}

func TestDecideCaps_OlderWaiterOnOtherLimitDoesNotBlock(t *testing.T) {
	t.Parallel()

	now := time.Now()
	req := ClaimRequest{Tenant: "acme", Cell: "cell-a", JobPath: "acme/app/build"}
	v := decideCaps("acme", req, uuid.Must(uuid.NewV7()), now, capTestLimit, 0,
		[]inFlightRun{capTestRun("app/test", "cell-a")},
		[]capWaiter{capTestWaiter("other", now.Add(-time.Minute))})
	if v.Blocked {
		t.Fatalf("a waiter on another limit must not block, got %+v", v)
	}
}

func TestDecideCaps_CellCapCountsOnlySameCell(t *testing.T) {
	t.Parallel()

	req := ClaimRequest{Tenant: "acme", Cell: "cell-a", JobPath: "acme/deploy/prod"}
	v := decideCaps("acme", req, uuid.Must(uuid.NewV7()), time.Now(), nil, 1,
		[]inFlightRun{capTestRun("deploy/prod", "cell-b")}, nil)
	if v.Blocked {
		t.Fatalf("a build on another cell must not count, got %+v", v)
	}
	v = decideCaps("acme", req, uuid.Must(uuid.NewV7()), time.Now(), nil, 1,
		[]inFlightRun{capTestRun("deploy/prod", "cell-a")}, nil)
	if !v.Blocked || v.Kind != capKindCell {
		t.Fatalf("a build on the same cell must block on the cell cap, got %+v", v)
	}
}

func TestDecideCaps_TenantLimitCheckedBeforeCell(t *testing.T) {
	t.Parallel()

	req := ClaimRequest{Tenant: "acme", Cell: "cell-a", JobPath: "acme/app/build"}
	v := decideCaps("acme", req, uuid.Must(uuid.NewV7()), time.Now(), []ConcurrencyLimit{{Name: "one", Jobs: []string{"app/*"}, Max: 1}}, 5,
		[]inFlightRun{capTestRun("app/test", "cell-a")}, nil)
	if !v.Blocked || v.Kind != capKindTenant {
		t.Fatalf("the tenant limit must answer first, got %+v", v)
	}
}
