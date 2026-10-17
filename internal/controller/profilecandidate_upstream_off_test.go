package controller

import (
	"context"
	"errors"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/pluginresolve"
)

// failOnRequestTransport fails the test when any request leaves the
// reconciler. It stands in for the upstream host when upstream reads are off.
type failOnRequestTransport struct {
	t *testing.T
}

func (f failOnRequestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.t.Errorf("upstream read attempted while upstream metadata is off: %s", req.URL)
	return nil, errors.New("request blocked by test")
}

// TestProfileCandidate_UpstreamDisabled_NoUpdateCenter verifies that a
// candidate with no update center and upstream reads off never sends a
// request. It stays Pending, records UpstreamMetadataDisabled, and requeues.
func TestProfileCandidate_UpstreamDisabled_NoUpdateCenter(t *testing.T) {
	client, candidate := newCandidateTestFixtures(t)

	rec := newCandidateReconciler(client, "")
	rec.httpDoer = &http.Client{Transport: failOnRequestTransport{t: t}}

	got, result := reconcileCandidate(t, rec, candidate.Name)
	// An unresolved candidate keeps an empty or Pending phase, as the other
	// retryable selectSource failures do.
	if p := got.Status.Phase; p != "" && p != v1alpha1.ProfileCandidatePhasePending {
		t.Fatalf("expected Phase Pending or empty, got %q", p)
	}
	if result.RequeueAfter != candidateRequeueAfter {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, candidateRequeueAfter)
	}
	cond := candidateCondition(got, v1alpha1.ConditionCandidateResolved)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "UpstreamMetadataDisabled" {
		t.Fatalf("expected Resolved=False/UpstreamMetadataDisabled, got %+v", cond)
	}
}

// TestProfileCandidate_UpstreamDisabled_OCIStoreStillResolves verifies that an
// update center with oci storage still selects the in-cluster source when
// upstream reads are off. The disabled error is returned only when no
// in-cluster source exists.
func TestProfileCandidate_UpstreamDisabled_OCIStoreStillResolves(t *testing.T) {
	client, _ := newCandidateTestFixtures(t)
	crdstore.MustSeed(client.store, &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: updateCenterSingletonName},
		Spec: v1alpha1.UpdateCenterSpec{
			Storage: v1alpha1.UpdateCenterStorage{
				Type: "oci",
				OCI:  &v1alpha1.UpdateCenterOCIStorage{Ref: "registry.internal/varroa/update-center"},
			},
		},
	})

	rec := newCandidateReconciler(client, "")
	rec.httpDoer = &http.Client{Transport: failOnRequestTransport{t: t}}
	rec.newRegistryStore = func(ref string, opts oci.RegistryOptions) (oci.BlobStore, error) {
		return nil, errors.New("registry unreachable in test")
	}

	_, _, err := rec.selectSource(context.Background(), "2.479", testLogger())
	if errors.Is(err, errUpstreamDisabled) {
		t.Fatal("oci update center must not report the upstream as disabled")
	}
	if !errors.Is(err, pluginresolve.ErrMetadataUnavailable) {
		t.Fatalf("expected ErrMetadataUnavailable from the unreachable registry, got %v", err)
	}
}
