package forgeops

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

// permClient adds the collaborator permission read to the fake forge client.
type permClient struct {
	*fakeClient
	permission string
	err        error
	login      string
}

func (p *permClient) CollaboratorPermission(_ context.Context, _ forge.Repo, login string) (string, error) {
	p.login = login
	return p.permission, p.err
}

func permReq(login string) forgeapi.PermissionRequest {
	return forgeapi.PermissionRequest{Tenant: "payments", Repo: testRepoURL, Login: login}
}

func TestPermission_ReturnsForgePermission(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := &permClient{fakeClient: pushable(), permission: "write"}
	oneWriter(fx, c.fakeClient)
	fx.clients[forge.NewToken([]byte("tok-a"))] = c
	rep := fx.ops.Permission(context.Background(), permReq("alice"))
	if !rep.OK || rep.Permission != "write" {
		t.Fatalf("reply %+v", rep)
	}
	if c.login != "alice" {
		t.Fatalf("login sent = %q, want alice", c.login)
	}
}

func TestPermission_InvalidLogin(t *testing.T) {
	for _, login := range []string{"", "a/b", "-lead", "has space", "averyveryveryveryveryveryveryveryverylongname"} {
		fx := newFixture(t, "agents-payments")
		noForgeCall(fx)
		rep := fx.ops.Permission(context.Background(), permReq(login))
		if rep.OK || rep.Code != forgeapi.CodeInvalidRequest {
			t.Fatalf("login %q: reply %+v, want invalid_request", login, rep)
		}
	}
}

func TestPermission_NoMatchingCredential(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	rep := fx.ops.Permission(context.Background(), permReq("alice"))
	if rep.OK || rep.Code != forgeapi.CodeNoMatchingCredential {
		t.Fatalf("reply %+v, want no_matching_credential", rep)
	}
}

func TestPermission_ClientWithoutReaderUnsupported(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	oneWriter(fx, pushable())
	rep := fx.ops.Permission(context.Background(), permReq("alice"))
	if rep.OK || rep.Code != forgeapi.CodeForgeUnsupported {
		t.Fatalf("reply %+v, want forge_unsupported", rep)
	}
}

func TestPermission_ForgeErrorMapped(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	c := &permClient{fakeClient: pushable(), err: errors.New("boom")}
	oneWriter(fx, c.fakeClient)
	fx.clients[forge.NewToken([]byte("tok-a"))] = c
	rep := fx.ops.Permission(context.Background(), permReq("alice"))
	if rep.OK || rep.Code != forgeapi.CodeForgeUnreachable {
		t.Fatalf("reply %+v, want forge_unreachable", rep)
	}
}
