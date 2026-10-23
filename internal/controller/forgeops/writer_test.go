package forgeops

import (
	"context"
	"errors"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/forge"
	"github.com/varroaci/varroa-jenkins/internal/forge/forgeapi"
)

func TestPickWriter_FirstPushingWins(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.addCred("sb", "b", "tok-b")
	fx.clients[forge.NewToken([]byte("tok-a"))] = &fakeClient{info: forge.Info{DefaultBranch: "main", Push: false}}
	fx.clients[forge.NewToken([]byte("tok-b"))] = pushable()
	creds, err := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	if err != nil {
		t.Fatal(err)
	}
	w, err := fx.ops.pickWriter(context.Background(), testHost, forge.Repo{Owner: "tenant", Name: "app"}, creds)
	if err != nil {
		t.Fatal(err)
	}
	if w.CredentialID != "b" || w.Info.DefaultBranch != "main" {
		t.Fatalf("writer %+v", w)
	}
}

func TestPickWriter_AllAuthFail(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.clients[forge.NewToken([]byte("tok-a"))] = &fakeClient{repoErr: forge.ErrAuth}
	creds, _ := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	_, err := fx.ops.pickWriter(context.Background(), testHost, forge.Repo{Owner: "tenant", Name: "app"}, creds)
	if got := codeOf(t, err); got != forgeapi.CodeForgeAuthFailed {
		t.Fatalf("code %q", got)
	}
}

func TestPickWriter_NoneMayPush(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.clients[forge.NewToken([]byte("tok-a"))] = &fakeClient{info: forge.Info{DefaultBranch: "main"}}
	creds, _ := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	_, err := fx.ops.pickWriter(context.Background(), testHost, forge.Repo{Owner: "tenant", Name: "app"}, creds)
	if got := codeOf(t, err); got != forgeapi.CodeForgeWriteDenied {
		t.Fatalf("code %q", got)
	}
}

func TestPickWriter_404CountsAsAuthed(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.secrets.addCred("sa", "a", "tok-a")
	fx.secrets.addCred("sb", "b", "tok-b")
	fx.clients[forge.NewToken([]byte("tok-a"))] = &fakeClient{repoErr: forge.ErrNotFound}
	fx.clients[forge.NewToken([]byte("tok-b"))] = &fakeClient{repoErr: forge.ErrAuth}
	creds, _ := fx.ops.chooseCredential(context.Background(), "payments", testHost)
	_, err := fx.ops.pickWriter(context.Background(), testHost, forge.Repo{Owner: "tenant", Name: "app"}, creds)
	if got := codeOf(t, err); got != forgeapi.CodeForgeWriteDenied {
		t.Fatalf("code %q", got)
	}
}

func TestPickWriter_Unsupported(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.detectE = forge.ErrUnsupported
	_, err := fx.ops.pickWriter(context.Background(), testHost, forge.Repo{Owner: "tenant", Name: "app"}, nil)
	if got := codeOf(t, err); got != forgeapi.CodeForgeUnsupported {
		t.Fatalf("code %q", got)
	}
}

func TestPickWriter_Unreachable(t *testing.T) {
	fx := newFixture(t, "agents-payments")
	fx.detectE = errors.New("dial tcp: i/o timeout")
	_, err := fx.ops.pickWriter(context.Background(), testHost, forge.Repo{Owner: "tenant", Name: "app"}, nil)
	if got := codeOf(t, err); got != forgeapi.CodeForgeUnreachable {
		t.Fatalf("code %q", got)
	}
}
