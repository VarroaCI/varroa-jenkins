package api

import (
	"context"
	"log/slog"
	"testing"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// clusterTriggerTransport is a fakeTriggerTransport that also addresses a
// named cluster's mite.
type clusterTriggerTransport struct {
	fakeTriggerTransport
	clusterCalls []string
}

func (f *clusterTriggerTransport) SendImperativeToCluster(_ context.Context, cluster, ns, name string, _ *mitev1.ImperativeCommand) error {
	f.clusterCalls = append(f.clusterCalls, cluster+"/"+ns+"/"+name)
	return nil
}

func clusterPublishServer(mt *clusterTriggerTransport) *Server {
	return NewServer(&Dependencies{
		Brood:        newFakeBrood(newFakeResourceClient()),
		MiteRegistry: mt,
		Logger:       slog.Default(),
	})
}

func TestPublishImperative_MemberClusterRefReachesThatCluster(t *testing.T) {
	mt := &clusterTriggerTransport{}
	srv := clusterPublishServer(mt)
	ref := TenantRef{Cluster: "member", Namespace: "varroa-system", Controller: "cell-m-0"}

	if err := srv.publishImperative(context.Background(), ref, &mitev1.ImperativeCommand{CommandId: "c"}); err != nil {
		t.Fatalf("publishImperative: %v", err)
	}
	if len(mt.clusterCalls) != 1 || mt.clusterCalls[0] != "member/varroa-system/cell-m-0" {
		t.Errorf("cluster sends = %v, want [member/varroa-system/cell-m-0]", mt.clusterCalls)
	}
	if n := len(mt.calls()); n != 0 {
		t.Errorf("local sends = %d, want 0", n)
	}
}

func TestPublishImperative_LocalRefKeepsTheLocalSend(t *testing.T) {
	for _, cluster := range []string{"", "core"} {
		mt := &clusterTriggerTransport{}
		srv := clusterPublishServer(mt)
		ref := TenantRef{Cluster: cluster, Namespace: "ns", Controller: "ci"}

		if err := srv.publishImperative(context.Background(), ref, &mitev1.ImperativeCommand{CommandId: "c"}); err != nil {
			t.Fatalf("cluster %q: publishImperative: %v", cluster, err)
		}
		if n := len(mt.calls()); n != 1 {
			t.Errorf("cluster %q: local sends = %d, want 1", cluster, n)
		}
		if len(mt.clusterCalls) != 0 {
			t.Errorf("cluster %q: cluster sends = %v, want none", cluster, mt.clusterCalls)
		}
	}
}

func TestPublishImperative_NoBroodUsesTheClusterSendForANamedCluster(t *testing.T) {
	mt := &clusterTriggerTransport{}
	srv := NewServer(&Dependencies{MiteRegistry: mt, Logger: slog.Default()})
	ref := TenantRef{Cluster: "member", Namespace: "ns", Controller: "ci"}

	if err := srv.publishImperative(context.Background(), ref, &mitev1.ImperativeCommand{CommandId: "c"}); err != nil {
		t.Fatalf("publishImperative: %v", err)
	}
	if len(mt.clusterCalls) != 1 || len(mt.calls()) != 0 {
		t.Errorf("cluster sends = %v, local sends = %d, want one cluster send", mt.clusterCalls, len(mt.calls()))
	}
}

func TestPublishImperative_MemberRefWithoutAClusterSenderFails(t *testing.T) {
	srv := NewServer(&Dependencies{
		Brood:        newFakeBrood(newFakeResourceClient()),
		MiteRegistry: &fakeTriggerTransport{},
		Logger:       slog.Default(),
	})
	ref := TenantRef{Cluster: "member", Namespace: "ns", Controller: "ci"}
	if err := srv.publishImperative(context.Background(), ref, &mitev1.ImperativeCommand{CommandId: "c"}); err == nil {
		t.Fatal("publishImperative: nil error, want a refusal for a transport that cannot reach another cluster")
	}
}
