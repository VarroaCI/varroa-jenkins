package pluginresolve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// TestUpstreamSource_PassesThroughToResolveSatisfying is a smoke test: it
// exercises no logic of UpstreamSource's own, only that it forwards to
// *ucmeta.Resolver.ResolveSatisfying unchanged.
func TestUpstreamSource_PassesThroughToResolveSatisfying(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plugins":{"mailer":{"version":"534.v9","sha256":"c2hh","requiredCore":"2.479.3"}}}`))
	}))
	t.Cleanup(srv.Close)

	resolver := ucmeta.NewResolver(func() []ucmeta.Source { return []ucmeta.Source{{URL: srv.URL}} }, time.Hour, nil, nil)
	src := UpstreamSource{Resolver: resolver}

	res := src.Resolve(context.Background(), "mailer", "500")
	if res.Outcome != ucmeta.Resolved {
		t.Fatalf("Outcome = %v, want Resolved", res.Outcome)
	}
	if res.Meta.Version != "534.v9" {
		t.Errorf("Meta.Version = %q, want 534.v9", res.Meta.Version)
	}

	if res := src.Resolve(context.Background(), "mailer", "600"); res.Outcome != ucmeta.NotListed {
		t.Errorf("Outcome = %v, want NotListed for a minimum the only version does not satisfy", res.Outcome)
	}
}
