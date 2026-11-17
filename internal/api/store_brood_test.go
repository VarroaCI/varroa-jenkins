package api

import (
	"context"
	"sort"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// storeBrood is a Brood for handler tests. Its local cluster core serves the
// Controllers of store, as the local operator does. Each name in members is a
// known cluster that has no Controllers. Each served call appends
// "<verb> <cluster>" to calls. A method that storeBrood does not serve panics.
type storeBrood struct {
	Brood
	store   crdstore.Backend
	members []string
	calls   []string
}

// newStoreBrood returns a storeBrood over store. store can be nil when the
// test reads no Controller through the brood.
func newStoreBrood(store crdstore.Backend, members ...string) *storeBrood {
	return &storeBrood{store: store, members: members}
}

func (b *storeBrood) LocalCluster() string { return "core" }

func (b *storeBrood) IsKnown(_ context.Context, cluster string) bool {
	if cluster == "core" {
		return true
	}
	for _, m := range b.members {
		if m == cluster {
			return true
		}
	}
	return false
}

func (b *storeBrood) Clusters(context.Context) ([]ClusterInfo, error) {
	out := make([]ClusterInfo, 0, 1+len(b.members))
	out = append(out, ClusterInfo{ClusterInfo: bus.ClusterInfo{Name: "core", State: bus.ClusterStateActive}, Core: true, Healthy: true})
	for _, m := range b.members {
		out = append(out, ClusterInfo{ClusterInfo: bus.ClusterInfo{Name: m, State: bus.ClusterStateActive}, Healthy: true})
	}
	return out, nil
}

func (b *storeBrood) StateOf(context.Context, string) (string, error) {
	return bus.ClusterStateActive, nil
}

func (b *storeBrood) ListAll(ctx context.Context, ns, filter string) ([]ClusterController, []ClusterFanoutStatus, error) {
	clusters := append([]string{"core"}, b.members...)
	if filter != "" {
		clusters = []string{filter}
	}
	rows := []ClusterController{}
	statuses := []ClusterFanoutStatus{}
	for _, c := range clusters {
		b.calls = append(b.calls, "list "+c)
		statuses = append(statuses, ClusterFanoutStatus{Name: c, OK: true})
		if c != "core" {
			continue
		}
		crs, err := crdstore.List[v1alpha1.Controller](ctx, b.store, ns, "")
		if err != nil {
			return nil, nil, err
		}
		sort.Slice(crs, func(i, j int) bool { return crs[i].Namespace+"/"+crs[i].Name < crs[j].Namespace+"/"+crs[j].Name })
		for _, cr := range crs {
			rows = append(rows, ClusterController{Cluster: c, CR: cr})
		}
	}
	return rows, statuses, nil
}

func (b *storeBrood) Get(ctx context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	b.calls = append(b.calls, "get "+cluster)
	if cluster != "core" {
		return nil, &BroodError{Code: bus.CodeNotFound, Msg: "controller not found"}
	}
	return crdstore.Get[v1alpha1.Controller](ctx, b.store, name, ns)
}

func (b *storeBrood) Delete(ctx context.Context, cluster, ns, name string) error {
	b.calls = append(b.calls, "delete "+cluster)
	if cluster != "core" {
		return &BroodError{Code: bus.CodeNotFound, Msg: "controller not found"}
	}
	return crdstore.Delete[v1alpha1.Controller](ctx, b.store, name, ns)
}

func (b *storeBrood) DeletePod(_ context.Context, cluster, _, _ string) error {
	b.calls = append(b.calls, "deletepod "+cluster)
	return nil
}
