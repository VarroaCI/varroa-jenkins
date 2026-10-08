package updatecenter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

func postServability(t *testing.T, baseURL string, body []byte) *http.Response {
	t.Helper()
	resp, err := http.Post(baseURL+"/api/v1/servability", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST servability: %v", err)
	}
	return resp
}

func decodeServability(t *testing.T, resp *http.Response) servabilityResponse {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out servabilityResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode servability response: %v", err)
	}
	return out
}

func servabilityBody(t *testing.T, plugins ...servabilityPlugin) []byte {
	t.Helper()
	b, err := json.Marshal(servabilityRequest{Plugins: plugins})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return b
}

func TestServability_PullThroughDisabled(t *testing.T) {
	ts := newTestServer(t, nil)
	resp := postServability(t, ts.baseURL, servabilityBody(t,
		servabilityPlugin{Name: "a", Version: "1"},
		servabilityPlugin{Name: "b", Version: "2"},
	))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out := decodeServability(t, resp)
	if len(out.Plugins) != 2 {
		t.Fatalf("answers = %d, want 2", len(out.Plugins))
	}
	for _, p := range out.Plugins {
		if p.Servable || p.Reason != servabilityReasonPullThroughDisabled {
			t.Errorf("%s@%s = %+v, want unservable pull-through-disabled", p.Name, p.Version, p)
		}
	}
}

func TestServability_MixedOutcomes(t *testing.T) {
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"plugins":{"a":{"version":"1","sha256":"c2hh","gav":"org.x:a:1"}}}`)
	}))
	t.Cleanup(meta.Close)
	arch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(arch.Close)

	r := ucmeta.NewResolver(func() []ucmeta.Source { return []ucmeta.Source{{URL: meta.URL}} }, time.Hour, nil, nil)
	r.SetArchiveBaseURL(arch.URL)
	ts := newTestServer(t, nil, WithPullThrough(true, meta.URL, meta.URL), WithMetadataResolver(r))

	resp := postServability(t, ts.baseURL, servabilityBody(t,
		servabilityPlugin{Name: "a", Version: "1"}, // metadata hit
		servabilityPlugin{Name: "b", Version: "9"}, // listed nowhere, no gav
		servabilityPlugin{Name: "a", Version: "0"}, // archive 404
	))
	out := decodeServability(t, resp)
	if len(out.Plugins) != 3 {
		t.Fatalf("answers = %d, want 3", len(out.Plugins))
	}
	if !out.Plugins[0].Servable || out.Plugins[0].Via != "metadata" {
		t.Errorf("a@1 = %+v, want servable via metadata", out.Plugins[0])
	}
	if out.Plugins[1].Servable || out.Plugins[1].Reason != servabilityReasonNotListed {
		t.Errorf("b@9 = %+v, want not-listed", out.Plugins[1])
	}
	if out.Plugins[2].Servable || out.Plugins[2].Reason != servabilityReasonNotListed {
		t.Errorf("a@0 = %+v, want not-listed", out.Plugins[2])
	}
}

func TestServability_RejectsOversizedBody(t *testing.T) {
	ts := newTestServer(t, nil)
	big := bytes.Repeat([]byte("x"), servabilityMaxBody+1)
	resp := postServability(t, ts.baseURL, big)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestServability_RejectsTooManyEntries(t *testing.T) {
	ts := newTestServer(t, nil)
	plugins := make([]servabilityPlugin, servabilityMaxEntries+1)
	for i := range plugins {
		plugins[i] = servabilityPlugin{Name: fmt.Sprintf("p%d", i), Version: "1"}
	}
	resp := postServability(t, ts.baseURL, servabilityBody(t, plugins...))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServability_MethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, nil)
	resp, err := http.Get(ts.baseURL + "/api/v1/servability")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

// inflightResolver records the highest number of lookups running at once.
type inflightResolver struct {
	cur, max int32
}

func (f *inflightResolver) ResolveAvailable(_ context.Context, _, _ string) ucmeta.Availability {
	n := atomic.AddInt32(&f.cur, 1)
	for {
		m := atomic.LoadInt32(&f.max)
		if n <= m || atomic.CompareAndSwapInt32(&f.max, m, n) {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	atomic.AddInt32(&f.cur, -1)
	return ucmeta.Availability{Outcome: ucmeta.Resolved, Via: ucmeta.ViaMetadata}
}

func TestServability_BoundsConcurrency(t *testing.T) {
	res := &inflightResolver{}
	plugins := make([]servabilityPlugin, 40)
	for i := range plugins {
		plugins[i] = servabilityPlugin{Name: fmt.Sprintf("p%d", i), Version: "1"}
	}
	results := make([]servabilityResult, len(plugins))
	(&Server{}).answerServability(context.Background(), res, plugins, results)
	if got := atomic.LoadInt32(&res.max); got > servabilityWorkers {
		t.Fatalf("max in-flight lookups = %d, want <= %d", got, servabilityWorkers)
	}
	for i, r := range results {
		if r.Name != plugins[i].Name || !r.Servable {
			t.Fatalf("result %d = %+v, want %s servable in request order", i, r, plugins[i].Name)
		}
	}
}

// TestServability_AgreesWithPullThrough: a pin the archive serves to /download must also
// answer servable via archive on the servability endpoint.
func TestServability_AgreesWithPullThrough(t *testing.T) {
	const name = "agedplug"
	const aged = "1.0"
	const current = "2.0"
	pluginBytes := []byte("aged plugin bytes")
	sum := sha256.Sum256(pluginBytes)
	sidecarHex := hex.EncodeToString(sum[:])

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"plugins":{%q:{"version":%q,"sha256":"d2Vla2x5","gav":"org.example:%s:%s"}}}`,
				name, current, name, current)
		case "/plugins/" + name + "/" + aged + "/" + name + ".hpi":
			_, _ = w.Write(pluginBytes)
		default:
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)

	sidecarPath := "/org/example/" + name + "/" + aged + "/" + name + "-" + aged + ".hpi.sha256"
	arch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sidecarPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(sidecarHex))
	}))
	t.Cleanup(arch.Close)

	r := ucmeta.NewResolver(func() []ucmeta.Source {
		return []ucmeta.Source{{URL: upstream.URL + "/update-center.actual.json"}}
	}, time.Hour, nil, nil)
	r.SetArchiveBaseURL(arch.URL)
	ts := newTestServer(t, nil, WithPullThrough(true, upstream.URL, upstream.URL), WithMetadataResolver(r))

	dl, err := http.Get(ts.baseURL + "/download/plugins/" + name + "/" + aged + "/" + name + ".hpi")
	if err != nil {
		t.Fatalf("GET download: %v", err)
	}
	body, _ := io.ReadAll(dl.Body)
	_ = dl.Body.Close()
	if dl.StatusCode != http.StatusOK || !bytes.Equal(body, pluginBytes) {
		t.Fatalf("download status = %d, want 200 with the upstream bytes", dl.StatusCode)
	}

	resp := postServability(t, ts.baseURL, servabilityBody(t, servabilityPlugin{Name: name, Version: aged}))
	out := decodeServability(t, resp)
	if len(out.Plugins) != 1 || !out.Plugins[0].Servable || out.Plugins[0].Via != "archive" {
		t.Fatalf("servability = %+v, want servable via archive", out.Plugins)
	}
}
