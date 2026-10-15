package bundle

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"
)

func TestInjectedVariableNames_OIDCClientSecretRemoved(t *testing.T) {
	// varroa_oidc_client_secret must NOT be in InjectedVariableNames, while
	// varroa_oidc_issuer and varroa_oidc_client_id must still be present.
	if slices.Contains(InjectedVariableNames, "varroa_oidc_client_secret") {
		t.Error("varroa_oidc_client_secret must not be in InjectedVariableNames after #411 fix")
	}
	if !slices.Contains(InjectedVariableNames, "varroa_oidc_issuer") {
		t.Error("varroa_oidc_issuer must remain in InjectedVariableNames")
	}
	if !slices.Contains(InjectedVariableNames, "varroa_oidc_client_id") {
		t.Error("varroa_oidc_client_id must remain in InjectedVariableNames")
	}
}

func TestMaterialize_StopsAtDeadline(t *testing.T) {
	for _, withCache := range []bool{false, true} {
		name := "cache off"
		if withCache {
			name = "cache on"
		}
		t.Run(name, func(t *testing.T) {
			remote := newSilentRemote(t)
			r := NewResolver(noop.NewMeterProvider(), t.TempDir())
			if withCache {
				r.UseCloneCache(newCacheForTest(t, t.TempDir(), 10, 1000))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()

			start := time.Now()
			_, err := r.Materialize(ctx, remote.url, ".", "main", filepath.Join(t.TempDir(), "clone"), nil)
			assertStoppedAtDeadline(t, err, time.Since(start))
			remote.assertPeerClosed(t)
		})
	}
}
