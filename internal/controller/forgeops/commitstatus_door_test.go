package forgeops

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// statusDoors is the door prefix of the cluster of statusRoot, and
// statusMovedRoot is the door of cell-b, the cell that a resume moved the run
// to.
const (
	statusDoors     = "https://varroa.example.org/headless/core/"
	statusMovedRoot = statusDoors + "cells/cell-b/"
)

// movedReq returns the reason request of build 7 after a resume moved the run
// from cell-a to cell-b. The action names cell-b.
func movedReq() forgeapi.CommitStatusRequest {
	req := endReq()
	req.Reason = "resume-failed"
	req.JenkinsURL = statusMovedRoot
	req.CellDoors = statusDoors
	return req
}

// seedController stores the controller cells/name, as a cell or as a
// headless controller that is not a cell.
func seedController(fx *fixture, name string, cell bool) {
	cr := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "cells"}}
	if cell {
		cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	} else {
		cr.Spec.Headless = true
	}
	crdstore.MustSeed(fx.store, cr)
}

// TestEndPendingStatus_ReasonAcrossCells covers a run that a resume moved
// from cell-a to cell-b. Cell-a wrote the pending status, and the action names
// cell-b. A reason request ends the status when cell-a is a cell or is gone.
// It leaves the status of a controller that is not a cell, of a controller
// that cannot be read, of a request with no door prefix, and of a result
// request.
func TestEndPendingStatus_ReasonAcrossCells(t *testing.T) {
	controllersGVR, err := crdstore.GVRFor[v1alpha1.Controller]()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		setup func(*fixture)
		req   func(*forgeapi.CommitStatusRequest)
		want  int
	}{
		{name: "first cell is a live cell", setup: func(fx *fixture) { seedController(fx, "cell-a", true) }, want: 1},
		{name: "first cell is gone", want: 1},
		{name: "headless controller that is not a cell", setup: func(fx *fixture) { seedController(fx, "cell-a", false) }},
		{name: "controller read fails", setup: func(fx *fixture) {
			fx.store.FailAlways("get", controllersGVR, errors.New("apiserver unavailable"))
		}},
		{name: "no door prefix", req: func(r *forgeapi.CommitStatusRequest) { r.CellDoors = "" }},
		{name: "result request", req: func(r *forgeapi.CommitStatusRequest) { r.Reason = ""; r.Result = "SUCCESS" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t, "agents-payments")
			if tc.setup != nil {
				tc.setup(fx)
			}
			c := pushable()
			c.statuses = []forge.CommitStatus{
				{Context: "ci/jenkins", State: forge.StatePending, TargetURL: statusBuildURL},
			}
			oneWriter(fx, c)
			req := movedReq()
			if tc.req != nil {
				tc.req(&req)
			}
			n, err := fx.ops.EndPendingStatus(context.Background(), req)
			if err != nil || n != tc.want || len(c.statusWrites) != tc.want {
				t.Fatalf("written %d, writes %+v, err %v; want %d", n, c.statusWrites, err, tc.want)
			}
			if tc.want == 1 {
				w := c.statusWrites[0]
				if w.TargetURL != statusBuildURL || w.State != forge.StateError || w.Description != "Varroa ended the build: resume-failed" {
					t.Fatalf("write = %+v", w)
				}
			}
		})
	}
}

// TestEndPendingStatus_ReasonLeavesOtherBuildsUnderOtherCells checks that the
// door match keeps the build number, the job, and the link tail rules. A
// pending status of build 8 under another cell, of another job under another
// cell, and of a page under the build stays pending.
func TestEndPendingStatus_ReasonLeavesOtherBuildsUnderOtherCells(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "other/number", State: forge.StatePending, TargetURL: statusRoot + "job/payments/job/app/job/main/8/"},
		{Context: "other/job", State: forge.StatePending, TargetURL: statusRoot + "job/payments/job/web/job/main/7/"},
		{Context: "other/page", State: forge.StatePending, TargetURL: statusRoot + "job/payments/job/app/job/main/7/console"},
		{Context: "ci/jenkins", State: forge.StatePending, TargetURL: statusBuildURL},
	}
	oneWriter(fx, c)
	n, err := fx.ops.EndPendingStatus(context.Background(), movedReq())
	if err != nil || n != 1 || len(c.statusWrites) != 1 || c.statusWrites[0].Context != "ci/jenkins" {
		t.Fatalf("written %d, writes %+v, err %v; want only ci/jenkins", n, c.statusWrites, err)
	}
}

// TestEndPendingStatus_LeavesPendingUnderAFormerOrigin checks a status that a
// cell wrote before a change of the dashboard origin. The operator compares
// only its current origin, so the status stays pending.
func TestEndPendingStatus_LeavesPendingUnderAFormerOrigin(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := pushable()
	c.statuses = []forge.CommitStatus{
		{Context: "ci/jenkins", State: forge.StatePending, TargetURL: "https://old.example.org/headless/core/cells/cell-a/job/payments/job/app/job/main/7/"},
	}
	oneWriter(fx, c)
	req := movedReq()
	req.JenkinsURL = statusRoot
	n, err := fx.ops.EndPendingStatus(context.Background(), req)
	if err != nil || n != 0 || len(c.statusWrites) != 0 {
		t.Fatalf("written %d, writes %+v, err %v; want none", n, c.statusWrites, err)
	}
}

// TestCellDoorBuild covers the door match with hostile links. Only a build URL
// of the request's job and build number under the door of a controller of
// the same cluster and origin matches.
func TestCellDoorBuild(t *testing.T) {
	const job = "payments/app/main"
	cases := []struct {
		name     string
		target   string
		tenant   string
		job      string
		wantDoor string
	}{
		{name: "another cell", target: statusDoors + "cells/cell-b/job/payments/job/app/job/main/7/", wantDoor: "cells/cell-b"},
		{name: "another cell with display redirect", target: statusDoors + "cells/cell-b/job/payments/job/app/job/main/7/display/redirect", wantDoor: "cells/cell-b"},
		{name: "a cell of another namespace", target: statusDoors + "pool-ns/cell-c/job/payments/job/app/job/main/7", wantDoor: "pool-ns/cell-c"},
		{name: "encoded branch slash", target: statusDoors + "cells/cell-b/job/payments/job/app/job/feature%2Fx/7/", job: "payments/app/feature%2Fx", wantDoor: "cells/cell-b"},
		{name: "doubly encoded branch slash", target: statusDoors + "cells/cell-b/job/payments/job/app/job/feature%252Fx/7/", job: "payments/app/feature%2Fx", wantDoor: "cells/cell-b"},
		{name: "another origin", target: "https://other.example.org/headless/core/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "another scheme", target: "http://varroa.example.org/headless/core/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "another port", target: "https://varroa.example.org:8443/headless/core/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "user info", target: "https://x@varroa.example.org/headless/core/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "another cluster segment", target: "https://varroa.example.org/headless/member-b/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "no headless segment", target: "https://varroa.example.org/core/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "another tenant's job with the same tail", target: statusDoors + "cells/cell-b/job/billing/job/payments/job/app/job/main/7/"},
		{name: "another tenant's folder", target: statusDoors + "cells/cell-b/job/billing/job/app/job/main/7/"},
		{name: "job path outside the tenant folder", target: statusDoors + "cells/cell-b/job/billing/job/app/job/main/7/", job: "billing/app/main"},
		{name: "classic controller link", target: "https://ci.example.org/job/payments/job/app/job/main/7/"},
		{name: "in-cluster endpoint", target: "http://cell-b-0123abcd-svc.cells.svc.cluster.local:8080/job/payments/job/app/job/main/7/"},
		{name: "encoded slash in the door", target: statusDoors + "cells%2Fcell-b/x/job/payments/job/app/job/main/7/"},
		{name: "dot segment in the door", target: statusDoors + "../core/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "upper case namespace", target: statusDoors + "CELLS/cell-b/job/payments/job/app/job/main/7/"},
		{name: "extra segment after the door", target: statusDoors + "cells/cell-b/extra/job/payments/job/app/job/main/7/"},
		{name: "door with no name", target: statusDoors + "cells/job/payments/job/app/job/main/7/"},
		{name: "another build number", target: statusDoors + "cells/cell-b/job/payments/job/app/job/main/8/"},
		{name: "prefix of the build number", target: statusDoors + "cells/cell-b/job/payments/job/app/job/main/70/"},
		{name: "a page under the build", target: statusDoors + "cells/cell-b/job/payments/job/app/job/main/7/console"},
		{name: "job url", target: statusDoors + "cells/cell-b/job/payments/job/app/job/main/"},
		{name: "no tenant", target: statusDoors + "cells/cell-b/job/payments/job/app/job/main/7/", tenant: "-"},
		{name: "url that does not parse", target: "https://[::1/headless/core/cells/cell-b/job/payments/job/app/job/main/7/"},
		{name: "empty target", target: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tenant, jobPath := "payments", job
			if tc.tenant == "-" {
				tenant = ""
			}
			if tc.job != "" {
				jobPath = tc.job
			}
			ns, name, ok := cellDoorBuild(tc.target, statusDoors, tenant, jobPath, 7)
			got := ""
			if ok {
				got = ns + "/" + name
			}
			if got != tc.wantDoor {
				t.Fatalf("cellDoorBuild(%q) = %q, %v; want %q", tc.target, got, ok, tc.wantDoor)
			}
		})
	}
	if _, _, ok := cellDoorBuild(statusDoors+"cells/cell-b/job/payments/job/app/job/main/7/", "", "payments", job, 7); ok {
		t.Fatal("cellDoorBuild with no door prefix matched")
	}
}
