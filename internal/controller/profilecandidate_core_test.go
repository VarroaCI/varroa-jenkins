package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/pluginresolve"
)

// TestCheckPluginsServable_LatestUsesCandidateCore checks that a latest pin is stored
// only when a stored version runs on the candidate's core.
func TestCheckPluginsServable_LatestUsesCandidateCore(t *testing.T) {
	var posted []ucServabilityPlugin
	ucSrv := candidateUCServer(t, []inventoryEntry{{Name: "foo", Version: "2.0", RequiredCore: "2.570"}}, nil, 0, &posted)
	client, _ := newCandidateTestFixtures(t)
	crdstore.MustSeed(client.store, &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: updateCenterSingletonName},
		Spec:       v1alpha1.UpdateCenterSpec{PullThrough: v1alpha1.UpdateCenterPullThrough{Enabled: true}},
	})
	rec := newCandidateReconciler(client, "")
	rec.ucBaseURL = ucSrv.URL

	closure := pluginresolve.Closure{Plugins: []pluginresolve.PluginPin{{ArtifactID: "foo", Version: "latest"}}}
	status := &v1alpha1.ProfileCandidateStatus{}
	if _, checked := rec.checkPluginsServable(context.Background(), closure, "2.555.3", status, testLogger()); !checked {
		t.Fatal("checkPluginsServable did not complete")
	}
	if len(posted) != 1 || posted[0] != (ucServabilityPlugin{Name: "foo", Version: "latest", Core: "2.555.3"}) {
		t.Errorf("posted = %+v, want foo latest core 2.555.3", posted)
	}
}
