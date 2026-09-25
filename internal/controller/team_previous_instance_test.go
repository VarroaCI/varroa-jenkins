package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// TestBuildCellCandidateCopiesThePreviousInstance asserts the placement
// pass receives the replaced pod the cell's status records.
func TestBuildCellCandidateCopiesThePreviousInstance(t *testing.T) {
	lastSeen := metav1.NewTime(time.Date(2026, 9, 25, 11, 25, 4, 0, time.UTC))
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells-ns"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status: v1alpha1.ControllerStatus{
			Cell: &v1alpha1.CellStatus{
				Instance:                 "uid-new",
				PreviousInstance:         "uid-old",
				PreviousInstanceLastSeen: &lastSeen,
			},
		},
	}

	cand := buildCellCandidate(cr, nil)
	if cand.Instance != "uid-new" || cand.PreviousInstance != "uid-old" {
		t.Fatalf("instance = %q, previous = %q, want uid-new and uid-old", cand.Instance, cand.PreviousInstance)
	}
	if cand.PreviousInstanceLastSeen == nil || !cand.PreviousInstanceLastSeen.Equal(lastSeen.Time) {
		t.Fatalf("PreviousInstanceLastSeen = %v, want %v", cand.PreviousInstanceLastSeen, lastSeen.Time)
	}
}

// TestFreshTeamReconcilerReportsTheCellPreviousInstance asserts a
// TeamReconciler with no announce cache, which is the state after an
// operator restart, reports the cell's replaced pod in its first announce.
//
// Without the entry the recorder never marks a run of the replaced pod,
// and the run stays RUNNING with no end.
func TestFreshTeamReconcilerReportsTheCellPreviousInstance(t *testing.T) {
	client := newTeamTestClient()
	client.namespaces["payments-ns"] = true
	seedJenkinsVersionProfile(client)

	lastSeen := metav1.NewTime(time.Now().Add(-5 * time.Minute).Truncate(time.Second))
	crdstore.MustSeed(client.store, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cell-a",
			Namespace: "cells-ns",
			Labels:    map[string]string{cells.LabelCellProfile: "jenkins-lts"},
		},
		Spec: v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 10}},
		Status: v1alpha1.ControllerStatus{
			Conditions: []v1alpha1.ControllerCondition{{Type: v1alpha1.ConditionCellReady, Status: metav1.ConditionTrue}},
			Cell: &v1alpha1.CellStatus{
				Tenants:                  []string{"payments"},
				AppliedTenants:           []string{"payments"},
				Instance:                 "uid-new",
				PreviousInstance:         "uid-old",
				PreviousInstanceLastSeen: &lastSeen,
			},
		},
	})

	team := &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "payments",
			UID:        types.UID("10000000-0000-0000-0000-000000000737"),
			Finalizers: []string{v1alpha1.TeamFinalizerTenantPlacement},
		},
		Spec: v1alpha1.TeamSpec{
			Members:    []string{"alice"},
			Namespaces: []string{"payments-ns"},
			Pipelines:  &v1alpha1.TeamPipelinesSpec{Profile: "jenkins-lts", AgentNamespace: "payments-ns"},
		},
		Status: v1alpha1.TeamStatus{
			Placement: &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell-a", CellNamespace: "cells-ns", Generation: 1},
			Pipelines: &v1alpha1.TeamPipelinesStatus{WebhookToken: "tok", WebhookSecret: "sec", AgentNamespace: "payments-ns"},
		},
	}
	client.teams["payments"] = team
	crdstore.MustSeed(client.store, team)

	rec := newPipelinesTeamReconciler(client, nil)
	var seen []cells.AnnounceRequest
	rec.SetSyncAnnouncer(func(_ context.Context, req cells.AnnounceRequest) (cells.AnnounceResult, error) {
		seen = append(seen, req)
		return cells.AnnounceResult{OK: true}, nil
	})

	if _, err := rec.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("the first reconcile sent no announce")
	}
	first := seen[0]
	if first.Cell != "cell-a" {
		t.Fatalf("the first announce names %q, want cell-a", first.Cell)
	}
	var entry *cells.DeadInstance
	for i := range first.DeadInstances {
		if first.DeadInstances[i].Instance == "uid-old" {
			entry = &first.DeadInstances[i]
		}
	}
	if entry == nil {
		t.Fatalf("the first announce reports no dead instance for uid-old: %+v", first.DeadInstances)
	}
	if entry.ConfirmedGoneAt == nil {
		t.Fatalf("the replaced pod carries no confirmation: %+v", entry)
	}
	if !entry.LastSeenAt.Equal(lastSeen.Time) {
		t.Fatalf("LastSeenAt = %v, want the recorded reading %v", entry.LastSeenAt, lastSeen.Time)
	}
}
