package controller

import (
	"context"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// SendImperativeCommand delivers cmd to the named controller's mite
// through the reconciler's own transport. It is the public entry point a
// caller outside this package uses to issue an imperative command. The
// ledger-action subscriber is the first such caller.
func (r *Reconciler) SendImperativeCommand(ctx context.Context, namespace, name string, cmd *mitev1.ImperativeCommand) error {
	return r.miteTransport.SendImperative(ctx, namespace, name, cmd)
}
