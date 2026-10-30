package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// lintMemberBrood reports the member cell that a member tenant names. Its
// Get answers the cell that the test sets.
type lintMemberBrood struct {
	Brood
	local string
	cell  *v1alpha1.Controller
}

func (b lintMemberBrood) LocalCluster() string { return b.local }

func (b lintMemberBrood) Get(context.Context, string, string, string) (*v1alpha1.Controller, error) {
	return b.cell, nil
}

// lintRelayBrood records cells.lint relays and answers the reply that the
// test sets.
type lintRelayBrood struct {
	ConfigBrood
	reply   bus.CellLintResponse
	err     error
	calls   int
	cluster string
	req     bus.CellLintRequest
}

func (f *lintRelayBrood) CellLint(_ context.Context, cluster string, req bus.CellLintRequest) (bus.CellLintResponse, error) {
	f.calls++
	f.cluster = cluster
	f.req = req
	return f.reply, f.err
}

// memberLintFixture places the lint tenant on the member cluster "east". The
// member cell starts in phase, and the relay answers reply and err.
func memberLintFixture(t *testing.T, phase v1alpha1.ControllerPhase, reply bus.CellLintResponse, err error) (lintFixture, *lintRelayBrood) {
	t.Helper()
	fx := newLintFixture(t, v1alpha1.ControllerPhaseConnected, lintSuccessReply, http.StatusOK)
	seedTenant(t, fx.client.crdStore, tenantCellObject("east", v1alpha1.TenantReasonTenantPlaced))
	cell := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: tenantCellName, Namespace: tenantCellNamespace},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status:     v1alpha1.ControllerStatus{Phase: phase},
	}
	fx.srv.deps.Brood = lintMemberBrood{local: "core", cell: cell}
	relay := &lintRelayBrood{reply: reply, err: err}
	fx.srv.deps.ConfigBrood = relay
	return fx, relay
}

// TestLintJenkinsfile_MemberClusterRelaysToMemberOperator asserts a member
// tenant is linted by its member operator, and that the local linter is not
// called.
func TestLintJenkinsfile_MemberClusterRelaysToMemberOperator(t *testing.T) {
	reply := bus.CellLintResponse{Errors: []bus.CellLintFinding{
		{Source: "jenkins", Line: 2, Column: 9, Message: "first"},
		{Source: "jenkins", Message: "second"},
	}}
	fx, relay := memberLintFixture(t, v1alpha1.ControllerPhaseConnected, reply, nil)
	body, serr := lintCall(t, fx, lintPlainTextBody)
	if serr != nil {
		t.Fatalf("serr = %+v", serr)
	}
	if relay.calls != 1 || relay.cluster != "east" {
		t.Fatalf("relay calls = %d cluster = %q, want 1 east", relay.calls, relay.cluster)
	}
	if relay.req.Cell != tenantCellName || relay.req.Namespace != tenantCellNamespace || relay.req.Jenkinsfile != lintPlainTextBody {
		t.Errorf("relay request = %+v", relay.req)
	}
	if fx.hits.path != "" {
		t.Errorf("the local linter was called for a member tenant")
	}
	if body.Valid {
		t.Error("valid = true, want false for relayed findings")
	}
	if len(body.Errors) < 2 || body.Errors[0].Message != "first" || body.Errors[0].Line != 2 || body.Errors[1].Message != "second" {
		t.Errorf("errors = %+v, want the two relayed findings first", body.Errors)
	}
}

// TestLintJenkinsfile_MemberHibernatedNeverRelays asserts the BFF refuses a
// hibernated member cell before it relays.
func TestLintJenkinsfile_MemberHibernatedNeverRelays(t *testing.T) {
	fx, relay := memberLintFixture(t, v1alpha1.ControllerPhaseHibernated, bus.CellLintResponse{}, nil)
	_, serr := lintCall(t, fx, lintPlainTextBody)
	if serr == nil || serr.Status != http.StatusConflict {
		t.Fatalf("serr = %+v, want 409", serr)
	}
	if relay.calls != 0 {
		t.Errorf("relay calls = %d, want 0", relay.calls)
	}
}

// TestLintJenkinsfile_MemberUnreachableAnswers502 asserts an unreachable
// member cluster answers 502.
func TestLintJenkinsfile_MemberUnreachableAnswers502(t *testing.T) {
	fx, _ := memberLintFixture(t, v1alpha1.ControllerPhaseConnected, bus.CellLintResponse{},
		&ErrClusterUnreachable{Cluster: "east", Err: errors.New("no responders")})
	_, serr := lintCall(t, fx, lintPlainTextBody)
	if serr == nil || serr.Status != http.StatusBadGateway {
		t.Fatalf("serr = %+v, want 502", serr)
	}
}

// TestLintJenkinsfile_MemberPhaseReplyAnswers409 asserts a member reply that
// names a non-ready phase answers the same refusal as a local cell.
func TestLintJenkinsfile_MemberPhaseReplyAnswers409(t *testing.T) {
	fx, _ := memberLintFixture(t, v1alpha1.ControllerPhaseConnected,
		bus.CellLintResponse{Phase: string(v1alpha1.ControllerPhaseHibernated), Code: bus.CodeConflict}, nil)
	_, serr := lintCall(t, fx, lintPlainTextBody)
	if serr == nil || serr.Status != http.StatusConflict {
		t.Fatalf("serr = %+v, want 409", serr)
	}
}

// TestLintJenkinsfile_MemberLinterErrorAnswers502 asserts a member cell whose
// linter could not answer is a 502 that carries the member's message.
func TestLintJenkinsfile_MemberLinterErrorAnswers502(t *testing.T) {
	fx, _ := memberLintFixture(t, v1alpha1.ControllerPhaseConnected,
		bus.CellLintResponse{LinterError: "the cell linter is unreachable", Code: bus.CodeInternal}, nil)
	_, serr := lintCall(t, fx, lintPlainTextBody)
	if serr == nil || serr.Status != http.StatusBadGateway || serr.Message != "the cell linter is unreachable" {
		t.Fatalf("serr = %+v, want 502 with the linter message", serr)
	}
}

// TestLintJenkinsfile_MemberReadyPhaseEchoIsNotValid asserts a member reply that
// echoes a ready phase is a 502, never a valid lint with no findings.
func TestLintJenkinsfile_MemberReadyPhaseEchoIsNotValid(t *testing.T) {
	fx, _ := memberLintFixture(t, v1alpha1.ControllerPhaseConnected,
		bus.CellLintResponse{Phase: string(v1alpha1.ControllerPhaseConnected), Code: bus.CodeConflict}, nil)
	body, serr := lintCall(t, fx, lintPlainTextBody)
	if serr == nil || serr.Status != http.StatusBadGateway {
		t.Fatalf("serr = %+v, body = %+v, want 502", serr, body)
	}
}

// TestLintJenkinsfile_MemberNeedsNoLocalSigner asserts a member tenant is
// linted without a BFF Jenkins token signer, because the member signs its own.
func TestLintJenkinsfile_MemberNeedsNoLocalSigner(t *testing.T) {
	fx, relay := memberLintFixture(t, v1alpha1.ControllerPhaseConnected, bus.CellLintResponse{Errors: []bus.CellLintFinding{}}, nil)
	fx.srv.deps.JenkinsTokenSigner = nil
	body, serr := lintCall(t, fx, lintPlainTextBody)
	if serr != nil {
		t.Fatalf("serr = %+v", serr)
	}
	if relay.calls != 1 {
		t.Errorf("relay calls = %d, want 1", relay.calls)
	}
	if !body.Valid && len(body.Errors) == 0 {
		t.Errorf("body = %+v, want no findings", body)
	}
}
