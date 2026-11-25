package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// controllerBrood serves Controllers from two fake stores, as the operators of
// the clusters would. The local cluster core reads and writes local. The member
// member-b reads and writes member. Each call appends "<verb> <cluster>" to
// calls. Create and Update are not served: a test that reaches them panics.
type controllerBrood struct {
	api.Brood
	local  *crdstore.Fake
	member *crdstore.Fake
	calls  []string
}

func (b *controllerBrood) LocalCluster() string { return "core" }

func (b *controllerBrood) IsKnown(_ context.Context, c string) bool {
	return c == "core" || c == "member-b"
}

// on returns the store of the operator of cluster.
func (b *controllerBrood) on(cluster string) (*crdstore.Fake, error) {
	switch cluster {
	case "core":
		return b.local, nil
	case "member-b":
		return b.member, nil
	}
	return nil, &api.ErrClusterUnreachable{Cluster: cluster, Err: errors.New("no responders")}
}

func (b *controllerBrood) ListAll(ctx context.Context, ns, filter string) ([]api.ClusterController, []api.ClusterFanoutStatus, error) {
	clusters := []string{"core", "member-b"}
	if filter != "" {
		clusters = []string{filter}
	}
	rows := []api.ClusterController{}
	statuses := []api.ClusterFanoutStatus{}
	for _, c := range clusters {
		b.calls = append(b.calls, "list "+c)
		store, err := b.on(c)
		if err != nil {
			return nil, nil, err
		}
		crs, err := crdstore.List[v1alpha1.Controller](ctx, store, ns, "")
		if err != nil {
			return nil, nil, err
		}
		for _, cr := range crs {
			rows = append(rows, api.ClusterController{Cluster: c, CR: cr})
		}
		statuses = append(statuses, api.ClusterFanoutStatus{Name: c, OK: true})
	}
	return rows, statuses, nil
}

func (b *controllerBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	b.calls = append(b.calls, "get "+cluster)
	store, err := b.on(cluster)
	if err != nil {
		return nil, err
	}
	return crdstore.Get[v1alpha1.Controller](ctx, store, name, ns)
}

func (b *controllerBrood) Delete(ctx context.Context, cluster, ns, name string) error {
	b.calls = append(b.calls, "delete "+cluster)
	store, err := b.on(cluster)
	if err != nil {
		return err
	}
	return crdstore.Delete[v1alpha1.Controller](ctx, store, name, ns)
}

// withControllerBrood wires a controllerBrood into deps. Its local cluster core
// serves the store of deps. The member store starts empty.
func withControllerBrood(deps *api.Dependencies) *controllerBrood {
	b := &controllerBrood{local: deps.Store.(*crdstore.Fake), member: crdstore.NewFake()}
	deps.Brood = b
	return b
}

// controllerToolCalls holds one call for each Controller tool that takes cluster.
func controllerToolCalls() map[string]map[string]any {
	return map[string]map[string]any{
		"list_controllers":     {"namespace": "ns"},
		"get_controller":       {"namespace": "ns", "name": "hibernated"},
		"create_controller":    {"namespace": "ns", "name": "new-one"},
		"update_controller":    {"namespace": "ns", "name": "hibernated", "version": "2.516.3"},
		"delete_controller":    {"namespace": "ns", "name": "hibernated"},
		"reconcile_controller": {"namespace": "ns", "name": "hibernated"},
		"restart_controller":   {"namespace": "ns", "name": "hibernated"},
		"hibernate_controller": {"namespace": "ns", "name": "hibernated"},
		"wake_controller":      {"namespace": "ns", "name": "hibernated"},
	}
}

// Without a brood, every Controller tool refuses the call on every cluster. The
// local store does not change, and no event is emitted.
func TestControllerTools_NoBroodRefusesTheCall(t *testing.T) {
	for tool, base := range controllerToolCalls() {
		for _, cluster := range []string{"", "core", "member-b"} {
			t.Run(tool+"/cluster="+cluster, func(t *testing.T) {
				deps := adminDeps()
				sink := &recordingSink{}
				deps.ActivityPublisher = sink
				deps.Reconciler = &guardReconciler{}
				args := map[string]any{}
				for k, v := range base {
					args[k] = v
				}
				if cluster != "" {
					args["cluster"] = cluster
				}
				tr := callWithClaims(t, deps, mcpAdminClaims, tool, args)
				if !tr.IsError || toolText(tr) != "brood not configured" {
					t.Fatalf("result = %q, want the brood refusal", toolText(tr))
				}
				if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), deps.Store, "hibernated", "ns"); err != nil {
					t.Errorf("the refused call changed the local store: %v", err)
				}
				if len(sink.events) != 0 {
					t.Errorf("events = %+v, want none", sink.events)
				}
			})
		}
	}
}

// An unknown cluster is refused before any brood or reconciler call, and no
// event is emitted.
func TestControllerTools_UnknownClusterRefusesTheCall(t *testing.T) {
	for tool, base := range controllerToolCalls() {
		t.Run(tool, func(t *testing.T) {
			deps := adminDeps()
			sink := &recordingSink{}
			deps.ActivityPublisher = sink
			b := withControllerBrood(deps)
			args := map[string]any{"cluster": "nowhere"}
			for k, v := range base {
				args[k] = v
			}
			tr := callWithClaims(t, deps, mcpAdminClaims, tool, args)
			if !tr.IsError || toolText(tr) != "unknown cluster: nowhere" {
				t.Fatalf("result = %q, want %q", toolText(tr), "unknown cluster: nowhere")
			}
			if len(b.calls) != 0 {
				t.Errorf("brood calls = %v, want none", b.calls)
			}
			if len(sink.events) != 0 {
				t.Errorf("events = %+v, want none", sink.events)
			}
		})
	}
}

// A get with a member cluster reads that member only.
func TestGetController_MemberClusterReadsTheMember(t *testing.T) {
	deps := adminDeps()
	b := withControllerBrood(deps)
	crdstore.MustSeed(b.member, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "ns"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.516.3"},
	})
	tr := callWithClaims(t, deps, mcpAdminClaims, "get_controller", map[string]any{
		"namespace": "ns", "name": "remote", "cluster": "member-b",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %s", toolText(tr))
	}
	if len(b.calls) != 1 || b.calls[0] != "get member-b" {
		t.Errorf("brood calls = %v, want one get on member-b", b.calls)
	}
}

// A delete with a member cluster goes through that member's operator only. The
// local store does not change, and the event names the member.
func TestDeleteController_MemberClusterLeavesTheLocalStore(t *testing.T) {
	deps := adminDeps()
	sink := &recordingSink{}
	deps.ActivityPublisher = sink
	b := withControllerBrood(deps)
	crdstore.MustSeed(b.member, &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "hibernated", Namespace: "ns"},
	})
	tr := callWithClaims(t, deps, mcpAdminClaims, "delete_controller", map[string]any{
		"namespace": "ns", "name": "hibernated", "cluster": "member-b",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %s", toolText(tr))
	}
	if len(b.calls) != 1 || b.calls[0] != "delete member-b" {
		t.Errorf("brood calls = %v, want one delete on member-b", b.calls)
	}
	if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), b.local, "hibernated", "ns"); err != nil {
		t.Errorf("a member delete changed the local store: %v", err)
	}
	if _, err := crdstore.Get[v1alpha1.Controller](context.Background(), b.member, "hibernated", "ns"); err == nil {
		t.Errorf("the member controller still exists")
	}
	if len(sink.events) != 1 || sink.events[0].Cluster != "member-b" {
		t.Errorf("events = %+v, want one event on member-b", sink.events)
	}
}

// A delete without cluster goes through the brood on the local cluster, and the
// event names core.
func TestDeleteController_LocalDeleteGoesThroughTheBrood(t *testing.T) {
	deps := adminDeps()
	sink := &recordingSink{}
	deps.ActivityPublisher = sink
	b := withControllerBrood(deps)
	tr := callWithClaims(t, deps, mcpAdminClaims, "delete_controller", map[string]any{
		"namespace": "ns", "name": "hibernated",
	})
	if tr.IsError {
		t.Fatalf("tool errored: %s", toolText(tr))
	}
	if len(b.calls) != 1 || b.calls[0] != "delete core" {
		t.Errorf("brood calls = %v, want one delete on core", b.calls)
	}
	if len(sink.events) != 1 || sink.events[0].Cluster != "core" {
		t.Errorf("events = %+v, want one event on core", sink.events)
	}
}

// noOperatorReconciler answers each controller action with the relay error of
// a request that no operator received.
type noOperatorReconciler struct{ *guardReconciler }

func (noOperatorReconciler) TriggerReconcile(context.Context, string, string, string) error {
	return fmt.Errorf("reconcile relay: %w", nats.ErrNoResponders)
}

func (noOperatorReconciler) ApproveRestart(context.Context, string, string, string, string, string) error {
	return fmt.Errorf("approve restart relay: %w", nats.ErrNoResponders)
}

func (noOperatorReconciler) Hibernate(context.Context, string, string, string) error {
	return fmt.Errorf("hibernate relay: %w", nats.ErrNoResponders)
}

func (noOperatorReconciler) Wake(context.Context, string, string, string) error {
	return fmt.Errorf("wake relay: %w", nats.ErrNoResponders)
}

// An action that no operator received names the missing operator of the
// target cluster, and no event is emitted.
func TestControllerActions_NoOperatorNamesTheOperator(t *testing.T) {
	for tool, verb := range map[string]string{
		"reconcile_controller": "reconcile",
		"restart_controller":   "restart",
		"hibernate_controller": "hibernate",
		"wake_controller":      "wake",
	} {
		t.Run(tool, func(t *testing.T) {
			deps := adminDeps()
			sink := &recordingSink{}
			deps.ActivityPublisher = sink
			withControllerBrood(deps)
			deps.Reconciler = noOperatorReconciler{&guardReconciler{}}
			tr := callWithClaims(t, deps, mcpAdminClaims, tool, map[string]any{
				"namespace": "ns", "name": "hibernated", "cluster": "member-b",
			})
			want := "failed to " + verb + " controller: no operator of cluster member-b received the request: the operator restarts or has no leader; try again later"
			if !tr.IsError || toolText(tr) != want {
				t.Fatalf("result = %q, want %q", toolText(tr), want)
			}
			if len(sink.events) != 0 {
				t.Errorf("events = %+v, want none", sink.events)
			}
		})
	}
}
