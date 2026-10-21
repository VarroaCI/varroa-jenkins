package controller

import mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"

// attachRepoGoneFolders sets the confirmed-gone folders of cellKey on a
// desired-state command. The field travels only with a rehydration bundle,
// because the mite reads it only while it applies that bundle.
func (r *Reconciler) attachRepoGoneFolders(desired *mitev1.DesiredStateCommand, cellKey string) {
	if desired.Rehydration == nil {
		return
	}
	desired.RehydrationGoneFolders = r.repoGoneFolders(cellKey)
}
