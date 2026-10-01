package controller

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// unconfirmedCache answers a cache whose last reply listed one
// unconfirmed instance on cell-2 and one on cell-3, in two namespaces.
func unconfirmedCache() *cells.SyncCache {
	seen := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	return &cells.SyncCache{Unconfirmed: []cells.DeadInstance{
		{Instance: "pod-a", LastSeenAt: seen, Cell: "cell-2", Namespace: "cells-ns"},
		{Instance: "pod-b", LastSeenAt: seen, Cell: "cell-3", Namespace: "other-ns"},
	}}
}

// TestConfirmDepartedInstances asserts the pod UID reading that confirms a
// departed instance. Only a successful list that holds no live pod with
// the instance's UID confirms it.
func TestConfirmDepartedInstances(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		phases map[types.UID]corev1.PodPhase
		err    error
		want   bool
	}{
		{name: "no pod with the uid confirms", phases: map[types.UID]corev1.PodPhase{"pod-new": corev1.PodRunning}, want: true},
		{name: "a live pod with the uid stays", phases: map[types.UID]corev1.PodPhase{"pod-a": corev1.PodRunning}},
		{name: "a pending pod with the uid stays", phases: map[types.UID]corev1.PodPhase{"pod-a": corev1.PodPending}},
		{name: "a failed pod with the uid confirms", phases: map[types.UID]corev1.PodPhase{"pod-a": corev1.PodFailed}, want: true},
		{name: "a succeeded pod with the uid confirms", phases: map[types.UID]corev1.PodPhase{"pod-a": corev1.PodSucceeded}, want: true},
		{name: "a failed list confirms nothing", err: errors.New("the api server is unreachable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTeamTestClient()
			client.podPhases = map[string]map[types.UID]corev1.PodPhase{"cells-ns": tc.phases}
			client.podPhasesErr = tc.err
			rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())
			cache := unconfirmedCache()
			cache.Unconfirmed = cache.Unconfirmed[:1]

			got := rec.confirmDepartedInstances(context.Background(), cache, now)
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("confirmed = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Instance != "pod-a" {
				t.Fatalf("confirmed = %+v, want pod-a alone", got)
			}
			if got[0].ConfirmedGoneAt == nil || !got[0].ConfirmedGoneAt.Equal(now) {
				t.Fatalf("confirmed_gone_at = %v, want %v", got[0].ConfirmedGoneAt, now)
			}
			if got[0].Cell != "cell-2" || got[0].Namespace != "cells-ns" {
				t.Fatalf("cell = %s/%s, want cells-ns/cell-2", got[0].Namespace, got[0].Cell)
			}
		})
	}
}

// TestConfirmDepartedInstancesListsEachNamespaceOnce asserts one list per
// namespace, and no list at all for a cache with no unconfirmed entry.
func TestConfirmDepartedInstancesListsEachNamespaceOnce(t *testing.T) {
	client := newTeamTestClient()
	client.podPhases = map[string]map[types.UID]corev1.PodPhase{}
	rec := NewTeamReconciler(client, client.store, teamTestNS, slog.Default())

	if got := rec.confirmDepartedInstances(context.Background(), &cells.SyncCache{}, time.Now()); got != nil {
		t.Fatalf("confirmed = %+v, want none", got)
	}
	if got := rec.confirmDepartedInstances(context.Background(), nil, time.Now()); got != nil {
		t.Fatalf("confirmed = %+v, want none", got)
	}
	if client.listPodPhasesCalls != 0 {
		t.Fatalf("lists = %d, want 0", client.listPodPhasesCalls)
	}

	cache := unconfirmedCache()
	cache.Unconfirmed = append(cache.Unconfirmed,
		cells.DeadInstance{Instance: "pod-c", Cell: "cell-4", Namespace: "cells-ns"})
	got := rec.confirmDepartedInstances(context.Background(), cache, time.Now())
	if len(got) != 3 {
		t.Fatalf("confirmed = %+v, want all three", got)
	}
	if client.listPodPhasesCalls != 2 {
		t.Fatalf("lists = %d, want one for each of the two namespaces", client.listPodPhasesCalls)
	}
}
