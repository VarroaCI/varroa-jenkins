package controller

import (
	"context"
	"fmt"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// cellSecretsVolumeName is the pod volume that carries a pool's confidential
// store.
const cellSecretsVolumeName = "varroa-cell-secrets"

// cellSecretsMountPath is where plugins-init reads the pool store. The
// container copies the files into $JENKINS_HOME/secrets/, because a Secret
// volume is read-only and Jenkins writes new keys at runtime.
const cellSecretsMountPath = "/run/secrets/varroa-cell-secrets"

// resolveCellSecretsName answers the pool confidential store a controller
// mounts (design D5). It applies four ordered tests: the controller is a
// cell, it is in the operator namespace, it carries a pool label, and the
// pool Secret holds all four keys and decrypts. The first three answer an
// empty name with no error, because a non-pool controller needs no store.
// The fourth answers an error, because a labelled pool cell without a usable
// store is a fault: a pod built on a private store breaks the pool's own
// guarantee. A key that does not decrypt is worse than an absent key,
// because Jenkins replaces it with a fresh per-cell value and reports
// nothing. The pool pass repairs an absent, an empty, and a repairable store
// within one tick, so those blocks clear on their own.
func (r *Reconciler) resolveCellSecretsName(ctx context.Context, cr *v1alpha1.Controller) (string, error) {
	if !v1alpha1.IsCell(cr) {
		return "", nil
	}
	if cr.Namespace != r.getOperatorNamespace() {
		return "", nil
	}
	pool := cr.Labels[cells.LabelCellPool]
	if pool == "" {
		return "", nil
	}
	name := cells.PoolSecretName(pool)
	data, err := r.client.GetSecret(ctx, name, cr.Namespace)
	if err != nil {
		return "", fmt.Errorf("read pool secret %q: %w", name, err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("pool secret %q holds no data", name)
	}
	for _, key := range cells.StoreKeys() {
		if len(data[key]) == 0 {
			return "", fmt.Errorf("pool secret %q lacks the key %q", name, key)
		}
	}
	if err := cells.VerifyStore(data); err != nil {
		return "", fmt.Errorf("pool secret %q does not verify: %w", name, err)
	}
	return name, nil
}
