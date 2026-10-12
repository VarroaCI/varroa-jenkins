package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// scriptApprovalKey is the JCasC path of the script-security approval list.
// JCasC replaces the whole list on each apply, so the operator writes the
// full union of the cell bundle and the pool.
const scriptApprovalKey = "approvedSignatures"

// injectPoolSignatures sets security.scriptApproval.approvedSignatures on
// the cell's jenkins.yaml. The value is the union of the bundle's own
// entries and the pool entries, deduplicated and sorted. It keeps every
// other security key. It always writes the key, as an empty list when no
// entry remains. An empty list keeps a removed signature from staying
// approved after a reload.
func injectPoolSignatures(jenkinsYAML string, pool []string) (string, error) {
	var doc map[string]any
	if strings.TrimSpace(jenkinsYAML) != "" {
		if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for pool signatures: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	security, _ := doc["security"].(map[string]any)
	if security == nil {
		security = map[string]any{}
		doc["security"] = security
	}
	approval, _ := security["scriptApproval"].(map[string]any)
	if approval == nil {
		approval = map[string]any{}
		security["scriptApproval"] = approval
	}

	seen := map[string]bool{}
	var union []string
	add := func(sig string) {
		if sig == "" || seen[sig] {
			return
		}
		seen[sig] = true
		union = append(union, sig)
	}
	existing, _ := approval[scriptApprovalKey].([]any)
	for _, e := range existing {
		if s, ok := e.(string); ok {
			add(s)
		}
	}
	for _, sig := range pool {
		if strings.Contains(sig, unresolvedVarToken) {
			continue
		}
		add(sig)
	}
	sort.Strings(union)

	list := make([]any, 0, len(union))
	for _, s := range union {
		list = append(list, s)
	}
	approval[scriptApprovalKey] = list

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after pool signature injection: %w", err)
	}
	return string(out), nil
}

// poolSignatures reads the approved signature list of the cell's pool. It
// answers nil for a controller with no pool label or no store. A read failure
// is an error. It must not read as a pool that no longer approves signatures,
// because that would revoke them in Jenkins.
func (r *Reconciler) poolSignatures(ctx context.Context, cr *v1alpha1.Controller) ([]string, error) {
	name := cr.Labels[cells.LabelCellPool]
	if name == "" || r.store == nil {
		return nil, nil
	}
	pool, err := crdstore.Get[v1alpha1.CellPool](ctx, r.store, name, "")
	switch {
	case err == nil:
		return pool.Spec.ApprovedSignatures, nil
	case apierrors.IsNotFound(err):
		return nil, nil
	default:
		return nil, fmt.Errorf("read cell pool %q: %w", name, err)
	}
}

// applyPoolSignatures renders the pool's approved script signatures onto a
// cell's resolved bundle. It runs after ResolveVars, like the pool
// libraries. It is a no-op for a controller that is not a cell.
func (r *Reconciler) applyPoolSignatures(ctx context.Context, cr *v1alpha1.Controller, mat *bundle.MaterializedBundle) error {
	if !isCellController(cr) {
		return nil
	}

	sigs, err := r.poolSignatures(ctx, cr)
	if err != nil {
		return err
	}

	withSigs, err := injectPoolSignatures(mat.JenkinsYAML, sigs)
	if err != nil {
		return err
	}
	mat.JenkinsYAML = withSigs
	return nil
}
