package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkins/declarativelint"
)

// cellLintMaxText bounds the Jenkinsfile text of one cells.lint request.
const cellLintMaxText = 256 << 10

// cellLintTokenTTL bounds the operator token of one lint call.
const cellLintTokenTTL = 60 * time.Second

// cellLintBudget bounds one linter call. The BFF relay waits longer, so the
// operator answers before the relay gives up.
const cellLintBudget = 22 * time.Second

// CellLinter answers cells.lint on this cluster's own cells.
type CellLinter interface {
	Lint(ctx context.Context, cr *v1alpha1.Controller, text string) ([]declarativelint.Finding, error)
}

// JenkinsCellLinter lints through a cell's Jenkins declarative linter with an
// operator token. Production sets ServiceURL to runners.CellServiceURL.
type JenkinsCellLinter struct {
	Signer interface {
		GenerateOperatorJenkinsToken(name, ns string, ttl time.Duration) (string, error)
	}
	ServiceURL func(*v1alpha1.Controller) string
}

// Lint mints a short operator token for the cell and calls its linter.
func (l *JenkinsCellLinter) Lint(ctx context.Context, cr *v1alpha1.Controller, text string) ([]declarativelint.Finding, error) {
	token, err := l.Signer.GenerateOperatorJenkinsToken(cr.Name, cr.Namespace, cellLintTokenTTL)
	if err != nil {
		return nil, err
	}
	return declarativelint.Validate(ctx, declarativelint.HTTPClient, l.ServiceURL(cr), token, text)
}

// HandleCellLint handles operator.<cluster>.cells.lint. It lints text on one
// cell of this cluster. It never wakes a cell: a cell that is not Connected or
// Running answers its phase and the linter is not called.
func (c *ConfigCRUD) HandleCellLint(data []byte) []byte {
	var req bus.CellLintRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Error: "invalid request", Code: bus.CodeInvalid})
	}
	if req.Namespace == "" || req.Cell == "" || req.Jenkinsfile == "" || len(req.Jenkinsfile) > cellLintMaxText {
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Error: "invalid request", Code: bus.CodeInvalid})
	}
	if c.Linter == nil {
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Error: "linter not configured", Code: bus.CodeInternal})
	}
	cr, err := crdstore.Get[v1alpha1.Controller](context.Background(), c.Store, req.Cell, req.Namespace)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Error: "cell not found", Code: bus.CodeNotFound})
		}
		c.Logger.Error("cells.lint read cell", "cell", req.Cell, "namespace", req.Namespace, "error", err)
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Error: "read cell failed", Code: bus.CodeInternal})
	}
	switch cr.Status.Phase {
	case v1alpha1.ControllerPhaseConnected, v1alpha1.ControllerPhaseRunning:
	default:
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Phase: string(cr.Status.Phase), Code: bus.CodeConflict})
	}

	ctx, cancel := context.WithTimeout(context.Background(), cellLintBudget)
	defer cancel()
	findings, err := c.Linter.Lint(ctx, cr, req.Jenkinsfile)
	if err != nil {
		cause := err
		var unreachable *declarativelint.UnreachableError
		if errors.As(err, &unreachable) {
			cause = unreachable.Err
		}
		c.Logger.Error("cells.lint linter", "cell", req.Cell, "namespace", req.Namespace, "error", cause)
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, LinterError: err.Error(), Code: bus.CodeInternal})
	}

	out := bus.CellLintResponse{Errors: make([]bus.CellLintFinding, 0, len(findings))}
	for _, f := range findings {
		out.Errors = append(out.Errors, bus.CellLintFinding{
			Source: f.Source, Rule: f.Rule, Line: f.Line, Column: f.Column, Message: f.Message,
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Error: "marshal error", Code: bus.CodeInternal})
	}
	if len(raw) > bus.ListPageBudget {
		return c.configReply(bus.CellLintResponse{Errors: []bus.CellLintFinding{}, Error: "lint reply too large", Code: bus.CodeInternal})
	}
	return raw
}
