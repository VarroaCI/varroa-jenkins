package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func init() { _ = v1alpha1.AddToScheme(scheme.Scheme) }

func testBroodScheme() *runtime.Scheme { return scheme.Scheme }

func TestCommandBroodOps_Create_TenancyInvalid(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	spec, _ := json.Marshal(v1alpha1.BroodOperationSpec{
		Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Targets: v1alpha1.BroodTargets{Names: []string{"other-ns/ctrl"}},
	})
	req, _ := json.Marshal(bus.BroodOpsCreateRequest{
		Namespace: "team-ns",
		Name:      "broodop-test-abcde",
		Spec:      spec,
		StartedBy: "admin",
	})

	resp := broodOps.HandleCreate(req)
	var reply bus.BroodOpsOpResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Code != bus.CodeInvalid {
		t.Fatalf("expected code %q, got %q", bus.CodeInvalid, reply.Code)
	}
}

func TestCommandBroodOps_Create_Success(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	spec, _ := json.Marshal(v1alpha1.BroodOperationSpec{
		Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}},
	})
	req, _ := json.Marshal(bus.BroodOpsCreateRequest{
		Namespace: "team-ns",
		Name:      "broodop-test-abcde",
		Spec:      spec,
		StartedBy: "admin",
	})

	resp := broodOps.HandleCreate(req)
	var reply bus.BroodOpsOpResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Code != "" {
		t.Fatalf("expected empty code, got %q: %s", reply.Code, reply.Error)
	}
	if reply.Op == nil {
		t.Fatal("expected op in reply")
	}

	// Verify the CR was created.
	var created v1alpha1.BroodOperation
	if err := json.Unmarshal(reply.Op, &created); err != nil {
		t.Fatalf("unmarshal op: %v", err)
	}
	if created.Name != "broodop-test-abcde" {
		t.Errorf("name = %q, want %q", created.Name, "broodop-test-abcde")
	}
	if created.Namespace != "team-ns" {
		t.Errorf("namespace = %q, want %q", created.Namespace, "team-ns")
	}
}

func TestCommandBroodOps_Create_AlreadyExists(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "broodop-test-abcde", Namespace: "team-ns"},
	}
	op.APIVersion = "varroa.dev/v1alpha1"
	op.Kind = "BroodOperation"
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).WithObjects(op).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	spec, _ := json.Marshal(v1alpha1.BroodOperationSpec{
		Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Targets: v1alpha1.BroodTargets{Names: []string{"ctrl-a"}},
	})
	req, _ := json.Marshal(bus.BroodOpsCreateRequest{
		Namespace: "team-ns",
		Name:      "broodop-test-abcde",
		Spec:      spec,
	})

	resp := broodOps.HandleCreate(req)
	var reply bus.BroodOpsOpResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Code != bus.CodeConflict {
		t.Fatalf("expected code %q, got %q", bus.CodeConflict, reply.Code)
	}
}

func TestCommandBroodOps_Get_NotFound(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	req, _ := json.Marshal(bus.BroodOpsGetRequest{Namespace: "ns", Name: "missing"})
	resp := broodOps.HandleGet(req)
	var reply bus.BroodOpsOpResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Code != bus.CodeNotFound {
		t.Fatalf("expected %q, got %q", bus.CodeNotFound, reply.Code)
	}
}

func TestCommandBroodOps_Cancel_NotFound(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	req, _ := json.Marshal(bus.BroodOpsCancelRequest{Namespace: "ns", Name: "missing"})
	resp := broodOps.HandleCancel(req)
	var reply bus.BroodOpsCancelResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Code != bus.CodeNotFound {
		t.Fatalf("expected %q, got %q", bus.CodeNotFound, reply.Code)
	}
}

func TestCommandBroodOps_Suspend_NotFound(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	req, _ := json.Marshal(bus.BroodOpsSuspendRequest{Namespace: "ns", Name: "missing", Suspend: true})
	resp := broodOps.HandleSuspend(req)
	var reply bus.BroodOpsOpResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Code != bus.CodeNotFound {
		t.Fatalf("expected %q, got %q", bus.CodeNotFound, reply.Code)
	}
}

func TestCommandBroodOps_Suspend_RoundTrip(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "broodop-test-abcde", Namespace: "team-ns"},
		Spec:       v1alpha1.BroodOperationSpec{Suspend: false},
	}
	op.APIVersion = "varroa.dev/v1alpha1"
	op.Kind = "BroodOperation"
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).WithObjects(op).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	req, _ := json.Marshal(bus.BroodOpsSuspendRequest{Namespace: "team-ns", Name: "broodop-test-abcde", Suspend: true})
	resp := broodOps.HandleSuspend(req)
	var reply bus.BroodOpsOpResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Code != "" {
		t.Fatalf("expected empty code, got %q: %s", reply.Code, reply.Error)
	}
	if reply.Op == nil {
		t.Fatal("expected op in reply")
	}
	var updated v1alpha1.BroodOperation
	if err := json.Unmarshal(reply.Op, &updated); err != nil {
		t.Fatalf("unmarshal op: %v", err)
	}
	if !updated.Spec.Suspend {
		t.Error("expected suspend=true after round-trip")
	}
}

func TestCommandBroodOps_List_StripsManagedFields(t *testing.T) {
	op := &v1alpha1.BroodOperation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "broodop-test-abcde",
			Namespace: "team-ns",
		},
	}
	op.APIVersion = "varroa.dev/v1alpha1"
	op.Kind = "BroodOperation"
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).WithObjects(op).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	req, _ := json.Marshal(bus.BroodOpsListRequest{})
	resp := broodOps.HandleList(req)
	var reply bus.BroodOpsListResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Code != "" {
		t.Fatalf("expected empty code, got %q: %s", reply.Code, reply.Error)
	}
	if len(reply.Ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(reply.Ops))
	}

	var decoded v1alpha1.BroodOperation
	if err := json.Unmarshal(reply.Ops[0], &decoded); err != nil {
		t.Fatalf("unmarshal op: %v", err)
	}
	if decoded.ManagedFields != nil {
		t.Error("expected managedFields to be stripped")
	}
}

// createBroodOpsForList stores count broodops named broodop-test-<i> in team-ns.
// Each carries an annotation of padBytes so tests can exceed the page budget.
func createBroodOpsForList(t *testing.T, cl client.Client, count, padBytes int) {
	t.Helper()
	pad := strings.Repeat("x", padBytes)
	for i := 0; i < count; i++ {
		op := &v1alpha1.BroodOperation{
			ObjectMeta: metav1.ObjectMeta{
				Name:        fmt.Sprintf("broodop-test-%d", i),
				Namespace:   "team-ns",
				Annotations: map[string]string{"pad": pad},
			},
		}
		op.APIVersion = "varroa.dev/v1alpha1"
		op.Kind = "BroodOperation"
		if err := cl.Create(context.TODO(), op); err != nil {
			t.Fatalf("create op: %v", err)
		}
	}
}

func TestCommandBroodOps_List_PagesAboveBudget(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())
	const count = 10
	createBroodOpsForList(t, cl, count, 100*1024)

	var names []string
	after := ""
	pages := 0
	for {
		req, _ := json.Marshal(bus.BroodOpsListRequest{Namespace: "team-ns", After: after})
		var reply bus.BroodOpsListResponse
		if err := json.Unmarshal(broodOps.HandleList(req), &reply); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if reply.Code != "" {
			t.Fatalf("page %d: code %q: %s", pages, reply.Code, reply.Error)
		}
		for _, raw := range reply.Ops {
			var op v1alpha1.BroodOperation
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("decode op: %v", err)
			}
			names = append(names, op.Name)
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
		t.Fatalf("got %d ops across pages, want %d", len(names), count)
	}
	for i, name := range names {
		if want := fmt.Sprintf("broodop-test-%d", i); name != want {
			t.Fatalf("op %d = %q, want %q", i, name, want)
		}
	}
}

func TestCommandBroodOps_List_AfterKey(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())
	createBroodOpsForList(t, cl, 3, 10)

	req, _ := json.Marshal(bus.BroodOpsListRequest{Namespace: "team-ns", After: "team-ns/broodop-test-0"})
	var reply bus.BroodOpsListResponse
	if err := json.Unmarshal(broodOps.HandleList(req), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Code != "" || len(reply.Ops) != 2 || reply.Next != "" {
		t.Fatalf("code=%q ops=%d next=%q, want 2 ops and no next", reply.Code, len(reply.Ops), reply.Next)
	}
}

func TestCommandBroodOps_Preview_Parity(t *testing.T) {
	ctrl := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ctrl", Namespace: "team-ns"},
		Status:     v1alpha1.ControllerStatus{Phase: v1alpha1.ControllerPhaseConnected},
	}
	ctrl.APIVersion = "varroa.dev/v1alpha1"
	ctrl.Kind = "Controller"
	cl := fake.NewClientBuilder().WithScheme(testBroodScheme()).WithObjects(ctrl).Build()
	broodOps := NewCommandBroodOps(cl, "operator-ns", slog.Default())

	spec, _ := json.Marshal(v1alpha1.BroodOperationSpec{
		Action:  v1alpha1.BroodAction{Verb: v1alpha1.BroodVerbReconcile},
		Targets: v1alpha1.BroodTargets{Names: []string{"my-ctrl"}},
	})
	req, _ := json.Marshal(bus.BroodOpsPreviewRequest{
		Namespace: "team-ns",
		Spec:      spec,
	})
	resp := broodOps.HandlePreview(req)
	var reply bus.BroodOpsPreviewResponse
	if err := json.Unmarshal(resp, &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reply.Code != "" {
		t.Fatalf("expected empty code, got %q: %s", reply.Code, reply.Error)
	}
	if len(reply.Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(reply.Targets))
	}
	if reply.Targets[0].Name != "my-ctrl" {
		t.Errorf("target name = %q, want %q", reply.Targets[0].Name, "my-ctrl")
	}
}
