package controller

import (
	"context"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// SendImperativeCommand delivers cmd to the named controller's mite
// through the reconciler's own transport. It is the public entry point a
// caller outside this package uses to issue an imperative command — the
// ledger-action subscriber (design D8a, task 9.1a) is the first one —
// mirroring MintMiteToken's role as the public door onto a private
// transport field.
func (r *Reconciler) SendImperativeCommand(ctx context.Context, namespace, name string, cmd *mitev1.ImperativeCommand) error {
	return r.miteTransport.SendImperative(ctx, namespace, name, cmd)
}
