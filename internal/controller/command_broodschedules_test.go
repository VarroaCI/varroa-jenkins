package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// createBroodSchedulesForList stores count brood schedules named
// sched-test-<i> in team-ns. Each carries a schedule string of padBytes so
// tests can exceed the page budget.
func createBroodSchedulesForList(t *testing.T, cl client.Client, count, padBytes int) {
	t.Helper()
	pad := strings.Repeat("x", padBytes)
	for i := 0; i < count; i++ {
		sched := &v1alpha1.BroodSchedule{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("sched-test-%d", i),
				Namespace: "team-ns",
			},
			Spec: v1alpha1.BroodScheduleSpec{Schedule: pad},
		}
		sched.APIVersion = "varroa.dev/v1alpha1"
		sched.Kind = "BroodSchedule"
		if err := cl.Create(context.TODO(), sched); err != nil {
			t.Fatalf("create schedule: %v", err)
		}
	}
}

func TestCommandBroodSchedules_List_PagesAboveBudget(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	scheds := NewCommandBroodSchedules(cl, "operator-ns", slog.Default())
	const count = 10
	createBroodSchedulesForList(t, cl, count, 100*1024)

	var names []string
	after := ""
	pages := 0
	for {
		req, _ := json.Marshal(bus.BroodScheduleListRequest{Namespace: "team-ns", After: after})
		var reply bus.BroodScheduleListResponse
		if err := json.Unmarshal(scheds.HandleList(req), &reply); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if reply.Error != "" {
			t.Fatalf("page %d: error %q", pages, reply.Error)
		}
		for _, raw := range reply.Items {
			var s bus.BroodScheduleResponse
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatalf("decode schedule: %v", err)
			}
			names = append(names, s.Name)
		}
		pages++
		if reply.Next == "" {
			break
		}
		after = reply.Next
		if pages > count {
			t.Fatal("paging did not end")
		}
	}
	if pages < 2 {
		t.Fatalf("expected more than one page, got %d", pages)
	}
	if len(names) != count {
		t.Fatalf("got %d schedules across pages, want %d", len(names), count)
	}
	for i, name := range names {
		if want := fmt.Sprintf("sched-test-%d", i); name != want {
			t.Fatalf("schedule %d = %q, want %q", i, name, want)
		}
	}
}

func TestCommandBroodSchedules_List_AfterKey(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	scheds := NewCommandBroodSchedules(cl, "operator-ns", slog.Default())
	createBroodSchedulesForList(t, cl, 3, 10)

	req, _ := json.Marshal(bus.BroodScheduleListRequest{Namespace: "team-ns", After: "team-ns/sched-test-0"})
	var reply bus.BroodScheduleListResponse
	if err := json.Unmarshal(scheds.HandleList(req), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Error != "" || len(reply.Items) != 2 || reply.Next != "" {
		t.Fatalf("error=%q items=%d next=%q, want 2 items and no next", reply.Error, len(reply.Items), reply.Next)
	}
}

func TestCommandBroodSchedules_List_RowTooLarge(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	scheds := NewCommandBroodSchedules(cl, "operator-ns", slog.Default())
	createBroodSchedulesForList(t, cl, 1, 950*1024)

	req, _ := json.Marshal(bus.BroodScheduleListRequest{Namespace: "team-ns"})
	var reply bus.BroodScheduleListResponse
	if err := json.Unmarshal(scheds.HandleList(req), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(reply.Error, "exceeds the list page budget") {
		t.Fatalf("error = %q, want a row-too-large error", reply.Error)
	}
	if len(reply.Items) != 0 {
		t.Fatalf("items = %d, want none", len(reply.Items))
	}
}
