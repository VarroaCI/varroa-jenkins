package controller

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/declarativelint"
)

// fakeCellLinter records each call and answers a fixed result.
type fakeCellLinter struct {
	calls    int
	findings []declarativelint.Finding
	err      error
}

func (f *fakeCellLinter) Lint(_ context.Context, _ *v1alpha1.Controller, _ string) ([]declarativelint.Finding, error) {
	f.calls++
	return f.findings, f.err
}

// lintCellStore seeds one cell in the given phase and returns a ConfigCRUD
// that lints through linter.
func lintCellStore(t *testing.T, phase v1alpha1.ControllerPhase, linter CellLinter) *ConfigCRUD {
	t.Helper()
	store := crdstore.NewFake()
	cr := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "cells"},
		Spec:       v1alpha1.ControllerSpec{Cell: &v1alpha1.CellSpec{Enabled: true}},
		Status:     v1alpha1.ControllerStatus{Phase: phase},
	}
	if err := store.Seed(cr); err != nil {
		t.Fatalf("seed cell: %v", err)
	}
	c := NewConfigCRUD(nil, store, nil, "varroa-system", slog.Default())
	c.Linter = linter
	return c
}

func lintRequest(t *testing.T, cell, text string) []byte {
	t.Helper()
	raw, err := json.Marshal(bus.CellLintRequest{Namespace: "cells", Cell: cell, Jenkinsfile: text})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

func decodeLintReply(t *testing.T, raw []byte) bus.CellLintResponse {
	t.Helper()
	var resp bus.CellLintResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode reply: %v (body %s)", err, raw)
	}
	return resp
}

// TestHandleCellLint_ConnectedCellAnswersFindings asserts a Connected cell
// answers the linter's findings.
func TestHandleCellLint_ConnectedCellAnswersFindings(t *testing.T) {
	linter := &fakeCellLinter{findings: []declarativelint.Finding{
		{Source: "jenkins", Line: 3, Column: 5, Message: "Unknown stage"},
	}}
	c := lintCellStore(t, v1alpha1.ControllerPhaseConnected, linter)
	resp := decodeLintReply(t, c.HandleCellLint(lintRequest(t, "cell-a", "pipeline {}")))
	if resp.Code != "" {
		t.Fatalf("code = %q, error = %q", resp.Code, resp.Error)
	}
	if linter.calls != 1 {
		t.Fatalf("linter calls = %d, want 1", linter.calls)
	}
	if len(resp.Errors) != 1 || resp.Errors[0].Line != 3 || resp.Errors[0].Column != 5 {
		t.Errorf("errors = %+v", resp.Errors)
	}
}

// TestHandleCellLint_RunningCellLints asserts a Running cell is linted too.
func TestHandleCellLint_RunningCellLints(t *testing.T) {
	linter := &fakeCellLinter{}
	c := lintCellStore(t, v1alpha1.ControllerPhaseRunning, linter)
	resp := decodeLintReply(t, c.HandleCellLint(lintRequest(t, "cell-a", "pipeline {}")))
	if resp.Code != "" || linter.calls != 1 {
		t.Errorf("code = %q, linter calls = %d", resp.Code, linter.calls)
	}
}

// TestHandleCellLint_HibernatedCellNeverCallsLinter asserts a cell that is not
// ready answers its phase and that the linter is not called.
func TestHandleCellLint_HibernatedCellNeverCallsLinter(t *testing.T) {
	linter := &fakeCellLinter{}
	c := lintCellStore(t, v1alpha1.ControllerPhaseHibernated, linter)
	resp := decodeLintReply(t, c.HandleCellLint(lintRequest(t, "cell-a", "pipeline {}")))
	if linter.calls != 0 {
		t.Errorf("linter calls = %d, want 0", linter.calls)
	}
	if resp.Phase != string(v1alpha1.ControllerPhaseHibernated) || resp.Code != bus.CodeConflict {
		t.Errorf("phase = %q code = %q, want Hibernated and conflict", resp.Phase, resp.Code)
	}
}

// TestHandleCellLint_UnknownCellAnswersNotFound asserts a missing cell answers
// not_found.
func TestHandleCellLint_UnknownCellAnswersNotFound(t *testing.T) {
	linter := &fakeCellLinter{}
	c := lintCellStore(t, v1alpha1.ControllerPhaseConnected, linter)
	resp := decodeLintReply(t, c.HandleCellLint(lintRequest(t, "missing", "pipeline {}")))
	if resp.Code != bus.CodeNotFound {
		t.Errorf("code = %q, want %q", resp.Code, bus.CodeNotFound)
	}
	if linter.calls != 0 {
		t.Errorf("linter calls = %d, want 0", linter.calls)
	}
}

// TestHandleCellLint_InvalidRequest asserts a malformed request, an empty
// field, and an oversize text each answer invalid.
func TestHandleCellLint_InvalidRequest(t *testing.T) {
	c := lintCellStore(t, v1alpha1.ControllerPhaseConnected, &fakeCellLinter{})
	cases := map[string][]byte{
		"malformed": []byte("{"),
		"empty":     lintRequest(t, "", "pipeline {}"),
		"oversize":  lintRequest(t, "cell-a", strings.Repeat("a", cellLintMaxText+1)),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			resp := decodeLintReply(t, c.HandleCellLint(raw))
			if resp.Code != bus.CodeInvalid {
				t.Errorf("code = %q, want %q", resp.Code, bus.CodeInvalid)
			}
		})
	}
}

// TestHandleCellLint_NoLinterAnswersInternal asserts an operator without a
// linter refuses the request as an internal error.
func TestHandleCellLint_NoLinterAnswersInternal(t *testing.T) {
	c := lintCellStore(t, v1alpha1.ControllerPhaseConnected, nil)
	resp := decodeLintReply(t, c.HandleCellLint(lintRequest(t, "cell-a", "pipeline {}")))
	if resp.Code != bus.CodeInternal {
		t.Errorf("code = %q, want %q", resp.Code, bus.CodeInternal)
	}
}

// TestHandleCellLint_LinterErrorCarriesMessage asserts a linter failure answers
// its message in LinterError.
func TestHandleCellLint_LinterErrorCarriesMessage(t *testing.T) {
	linter := &fakeCellLinter{err: errors.New("the cell linter is unreachable")}
	c := lintCellStore(t, v1alpha1.ControllerPhaseConnected, linter)
	resp := decodeLintReply(t, c.HandleCellLint(lintRequest(t, "cell-a", "pipeline {}")))
	if resp.LinterError != "the cell linter is unreachable" || resp.Code != bus.CodeInternal {
		t.Errorf("linterError = %q code = %q", resp.LinterError, resp.Code)
	}
}

// TestHandleCellLint_ValidLintMarshalsEmptyErrors asserts a valid lint sends
// an empty errors array, not null.
func TestHandleCellLint_ValidLintMarshalsEmptyErrors(t *testing.T) {
	c := lintCellStore(t, v1alpha1.ControllerPhaseConnected, &fakeCellLinter{})
	raw := c.HandleCellLint(lintRequest(t, "cell-a", "pipeline {}"))
	if !strings.Contains(string(raw), `"errors":[]`) {
		t.Errorf("reply = %s, want an empty errors array", raw)
	}
}
