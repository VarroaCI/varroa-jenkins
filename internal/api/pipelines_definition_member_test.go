package api

import (
	"context"
	"io"
	"log/slog"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// memberTeamBrood answers GetTeamBundleContent from a map keyed by
// cluster/team. It records each member call. Every other ConfigBrood method
// is unused by these reads and panics if reached.
type memberTeamBrood struct {
	ConfigBrood
	replies map[string]memberTeamReply
	calls   []string
}

type memberTeamReply struct {
	cb      *v1alpha1.ComposedBundle
	content map[string]string
	held    bool
	err     error
}

func (f *memberTeamBrood) GetTeamBundleContent(_ context.Context, cluster, team string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	key := cluster + "/" + team
	f.calls = append(f.calls, key)
	r, ok := f.replies[key]
	if !ok {
		return nil, nil, false, &BroodError{Code: bus.CodeNotFound, Msg: "team not found"}
	}
	return r.cb, r.content, r.held, r.err
}

// memberTeamRef is a placed Team that member-a holds and the core does not.
var memberTeamRef = TenantRef{
	Cluster: "member-a", Namespace: "cells", Controller: "cell-a",
	Team: "payments", Tenant: "payments", Folder: "payments",
}

func memberTeamServer(t *testing.T, brood *memberTeamBrood) *Server {
	t.Helper()
	client := newFakeResourceClient()
	srv := NewServer(&Dependencies{
		Store:             storeFromFake(client),
		Client:            client,
		Brood:             newFakeBrood(client),
		ConfigBrood:       brood,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		OperatorNamespace: "test-ns",
	})
	return srv
}

func memberBundle() *v1alpha1.ComposedBundle {
	return &v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: "payments-bundle", Namespace: "cells"}}
}

func TestControllerItemManifest_MemberPlacedTeamReadsMemberBundle(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{"items.yaml": teamItemsYAML}},
	}}
	srv := memberTeamServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTeamRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil || len(manifest.Items) != 1 {
		t.Fatalf("manifest = %+v, want one re-rooted folder", manifest)
	}
	root := manifest.Items[0]
	if root.Kind != "folder" || root.Name != "payments" || len(root.Items) == 0 {
		t.Fatalf("root = %s %q with %d items, want folder payments holding the member's items", root.Kind, root.Name, len(root.Items))
	}
}

func TestControllerItemManifest_MemberPlacedTeamUnknownKeyGivesNoManifest(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{"items.yaml": tenantUnknownKeyItems}},
	}}
	srv := memberTeamServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTeamRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("manifest = %+v, want nil: the strict parse refuses an unknown key", manifest)
	}
}

func TestControllerItemManifest_MemberPlacedTeamVarInKeyKeepsManifest(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {cb: memberBundle(), content: map[string]string{
			"items.yaml":     tenantVarInKeyItems,
			"variables.yaml": "field: builders\n",
		}},
	}}
	srv := memberTeamServer(t, brood)

	_, _, manifest, err := srv.controllerItemManifest(context.Background(), memberTeamRef)
	if err != nil {
		t.Fatalf("controllerItemManifest: %v", err)
	}
	if manifest == nil || len(manifest.Items) != 1 {
		t.Fatalf("manifest = %+v, want one item after variable resolution", manifest)
	}
}

func TestTeamPipelineBundleContent_MemberNotFoundAnswersNil(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {err: &BroodError{Code: bus.CodeNotFound, Msg: "team not found"}},
	}}
	srv := memberTeamServer(t, brood)

	cb, content, _, err := srv.teamPipelineBundleContent(context.Background(), memberTeamRef)
	if cb != nil || content != nil || err != nil {
		t.Fatalf("got (%v, %v, %v), want nil, nil, nil", cb, content, err)
	}
}

func TestTeamPipelineBundleContent_MemberUnreachableReturnsError(t *testing.T) {
	brood := &memberTeamBrood{replies: map[string]memberTeamReply{
		"member-a/payments": {err: &ErrClusterUnreachable{Cluster: "member-a"}},
	}}
	srv := memberTeamServer(t, brood)

	if _, _, _, err := srv.teamPipelineBundleContent(context.Background(), memberTeamRef); err == nil {
		t.Fatal("err = nil, want the unreachable member error")
	}
}

func TestTeamPipelineBundleContent_LocalClusterNotFoundDoesNotCallMember(t *testing.T) {
	brood := &memberTeamBrood{}
	srv := memberTeamServer(t, brood)

	localRef := memberTeamRef
	localRef.Cluster = "core"
	cb, content, _, err := srv.teamPipelineBundleContent(context.Background(), localRef)
	if cb != nil || content != nil || err != nil {
		t.Fatalf("got (%v, %v, %v), want nil, nil, nil", cb, content, err)
	}
	if len(brood.calls) != 0 {
		t.Fatalf("member calls = %v, want none for the local cluster", brood.calls)
	}
}
