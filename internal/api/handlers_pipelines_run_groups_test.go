package api

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// groupsNow is the reference time of the run-group tests.
var groupsNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// TestListFleetRunGroupsDefaultSince asserts the since value defaults to 24 hours
// before now, and the limit to 100.
func TestListFleetRunGroupsDefaultSince(t *testing.T) {
	p, err := ParseFleetRunGroupsQuery(url.Values{}, groupsNow)
	if err != nil || !p.Since.Equal(groupsNow.Add(-24*time.Hour)) || p.Limit != 100 {
		t.Fatalf("default = %+v, %v; want since now-24h and limit 100", p, err)
	}
}

// TestParseFleetRunGroupsQueryRefusesOldSince asserts a since value older than 48
// hours is refused.
func TestParseFleetRunGroupsQueryRefusesOldSince(t *testing.T) {
	if _, err := ParseFleetRunGroupsQuery(url.Values{"since": {"72h"}}, groupsNow); err == nil {
		t.Fatal("a 72 hour since was accepted; want a refusal")
	}
	if _, err := ParseFleetRunGroupsQuery(url.Values{"since": {"47h"}}, groupsNow); err != nil {
		t.Fatalf("a 47 hour since err = %v; want it accepted", err)
	}
}

// TestParseFleetRunGroupsQueryRefusesFutureSince asserts a since value after now
// is refused with the past-only message.
func TestParseFleetRunGroupsQueryRefusesFutureSince(t *testing.T) {
	future := groupsNow.Add(time.Hour).Format(time.RFC3339)
	if _, err := ParseFleetRunGroupsQuery(url.Values{"since": {future}}, groupsNow); err == nil || err.Error() != "since must be in the past" {
		t.Fatalf("future since err = %v; want the past-only refusal", err)
	}
}

// TestParseFleetRunGroupsQueryLimit asserts the limit accepts 1 to 200 and refuses
// any other value.
func TestParseFleetRunGroupsQueryLimit(t *testing.T) {
	for _, raw := range []string{"0", "201", "abc"} {
		if _, err := ParseFleetRunGroupsQuery(url.Values{"limit": {raw}}, groupsNow); err == nil {
			t.Fatalf("limit %q was accepted; want a refusal", raw)
		}
	}
	p, err := ParseFleetRunGroupsQuery(url.Values{"limit": {"200"}}, groupsNow)
	if err != nil || p.Limit != 200 {
		t.Fatalf("limit 200 = %+v, %v; want 200", p, err)
	}
}

// TestListFleetRunGroupsNoScopeReadsNothing asserts a caller with no readable
// tenant answers an empty page and the store is not read.
func TestListFleetRunGroupsNoScopeReadsNothing(t *testing.T) {
	reader := &fakeRecordReader{groupsPage: recordstore.RunGroupsPage{Held: 3}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader)
	page, serviceErr, err := srv.ListFleetRunGroups(context.Background(), resolverClaims, FleetRunGroupsParams{Since: groupsNow.Add(-time.Hour), Limit: 100}, groupsNow)
	if serviceErr != nil || err != nil {
		t.Fatalf("ListFleetRunGroups: service %+v, err %v", serviceErr, err)
	}
	if len(reader.groupsQueries) != 0 {
		t.Fatalf("RunGroups calls = %d, want none", len(reader.groupsQueries))
	}
	if page.Groups == nil || len(page.Groups) != 0 || page.Held != 0 {
		t.Fatalf("page = %+v, want an empty non-nil group list and no held count", page)
	}
}

// TestListFleetRunGroupsStopsWhenPlacementsFail asserts a degraded placement read
// stops the groups before the store is read.
func TestListFleetRunGroupsStopsWhenPlacementsFail(t *testing.T) {
	reader := &fakeRecordReader{placementsErr: context.DeadlineExceeded}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	_, serviceErr, err := srv.ListFleetRunGroups(context.Background(), resolverClaims, FleetRunGroupsParams{Since: groupsNow.Add(-time.Hour), Limit: 100}, groupsNow)
	if err == nil && serviceErr == nil {
		t.Fatal("ListFleetRunGroups answered without an error after a failed placement read")
	}
	if len(reader.groupsQueries) != 0 {
		t.Fatalf("RunGroups calls = %d, want none", len(reader.groupsQueries))
	}
}

// TestListFleetRunGroupsMapsRailNames asserts a group names its rail tenant and
// its folder, and the held count and more flag reach the body.
func TestListFleetRunGroupsMapsRailNames(t *testing.T) {
	now := groupsNow
	newest := attentionRun("00000000-0000-4000-8000-000000000003", "COMPLETED", "pay/build", now.Add(-time.Hour))
	success := "SUCCESS"
	newest.Result = &success
	reader := &fakeRecordReader{groupsPage: recordstore.RunGroupsPage{
		Groups: []recordstore.RunGroup{{Cluster: "core", Tenant: "pay", JobPath: "pay/build", Runs: 2, Passed: 2, FirstAt: now.Add(-2 * time.Hour), Newest: newest}},
		Held:   1,
		More:   true,
	}}
	srv := fleetServer(t, globalPipelinesAuthorizer(), reader, pipelinesTenant("pay", true, "core", "ns", "c1"))
	p := FleetRunGroupsParams{Since: now.Add(-24 * time.Hour), Limit: 100}
	page, serviceErr, err := srv.ListFleetRunGroups(t.Context(), resolverClaims, p, now)
	if serviceErr != nil || err != nil {
		t.Fatalf("ListFleetRunGroups: service %+v, err %v", serviceErr, err)
	}
	if page.Groups == nil || len(page.Groups) != 1 {
		t.Fatalf("groups = %+v, want one non-nil group", page.Groups)
	}
	g := page.Groups[0]
	if g.Tenant != "pay" || g.JobPath != "pay/build" || g.Runs != 2 || g.Passed != 2 || g.Newest.RunID != newest.RunID {
		t.Fatalf("group = %+v, want pay/build with 2 passed runs", g)
	}
	if page.Held != 1 || !page.More {
		t.Fatalf("held = %d, more = %v; want 1 and true", page.Held, page.More)
	}
}

// TestListFleetRunGroupsNoRecordStore asserts a missing store answers 503.
func TestListFleetRunGroupsNoRecordStore(t *testing.T) {
	srv := fleetServer(t, globalPipelinesAuthorizer(), nil, pipelinesTenant("pay", true, "core", "ns", "c1"))
	_, serviceErr, _ := srv.ListFleetRunGroups(t.Context(), resolverClaims, FleetRunGroupsParams{}, time.Now().UTC())
	if serviceErr == nil || serviceErr.Status != 503 {
		t.Fatalf("service error = %+v, want 503", serviceErr)
	}
}
