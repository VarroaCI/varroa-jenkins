package controller

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// departedTestStatus places a tenant on cell-0 with a second member on
// cell-2, which is the shape the promotion check ran on core.
func departedTestStatus() *v1alpha1.TenantStatus {
	return &v1alpha1.TenantStatus{
		Placement: &v1alpha1.TenantPlacementStatus{
			Cell:          "cell-0",
			CellNamespace: "cells-ns",
			Members: []v1alpha1.TenantMemberStatus{
				{Cell: "cell-0", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive},
				{Cell: "cell-2", CellNamespace: "cells-ns", Role: v1alpha1.TenantMemberRoleActive},
			},
		},
	}
}

// liveCells names cell-0 alone, so cell-2 reads as departed.
func liveCells() map[string]*v1alpha1.Controller {
	return map[string]*v1alpha1.Controller{
		"cells-ns/cell-0": {ObjectMeta: metav1.ObjectMeta{Name: "cell-0", Namespace: "cells-ns"}},
	}
}

// TestDepartedMemberPods asserts the pod reading of a departed member cell.
// The stamp that arms a takeover of that member's runs stands on the
// reading, so a transient read error must never answer a confirmed
// absence.
func TestDepartedMemberPods(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "controllers"}, "cell-2")
	cases := []struct {
		name string
		pod  *corev1.Pod
		err  error
		want bool
	}{
		{name: "no pod answers gone", want: true},
		// The Controller can go while its pod still drains, so only the
		// pod UID read confirms such an instance.
		{name: "a deleted controller answers present", err: notFound},
		{
			name: "a running pod answers present",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		},
		{
			name: "an exited pod answers gone",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			want: true,
		},
		{name: "a failed read answers present", err: errors.New("the api server is unreachable")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTenantTestClient()
			client.controllerPod = tc.pod
			client.controllerPodErr = tc.err
			rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())

			got := rec.departedMemberPods(context.Background(), departedTestStatus(), liveCells())
			if len(got) != 1 {
				t.Fatalf("readings = %v, want one for the departed cell alone", got)
			}
			if got["cells-ns/cell-2"] != tc.want {
				t.Fatalf("cell-2 reads gone = %v, want %v", got["cells-ns/cell-2"], tc.want)
			}
		})
	}
}

// TestDepartedMemberPodsReadsNoLiveCell asserts a tenant whose members all
// hold live cells costs no pod read.
func TestDepartedMemberPodsReadsNoLiveCell(t *testing.T) {
	client := newTenantTestClient()
	rec := NewTenantReconciler(client, client.store, tenantTestNS, slog.Default())
	status := departedTestStatus()
	status.Placement.Members = status.Placement.Members[:1]

	got := rec.departedMemberPods(context.Background(), status, liveCells())
	if len(got) != 0 {
		t.Fatalf("readings = %v, want none", got)
	}
	if client.getControllerPodCalls != 0 {
		t.Fatalf("pod reads = %d, want 0", client.getControllerPodCalls)
	}
}
