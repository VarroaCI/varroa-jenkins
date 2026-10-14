package updatecenter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// prefetchUpstream serves one plugin's metadata and HPI. sha overrides the checksum
// the metadata advertises; pass "" to advertise the real checksum.
func prefetchUpstream(t *testing.T, name, version string, hpi []byte, sha string, hpiCalls *int) *httptest.Server {
	t.Helper()
	if sha == "" {
		sha = sha256Base64(hpi)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{
				"updateCenterVersion": "1",
				"core": {"name": "core", "version": "2.516.1", "url": "https://example.test/jenkins.war", "sha256": "Zm9v"},
				"plugins": {
					%q: {"name": %q, "version": %q, "sha256": %q, "dependencies": []}
				}
			}`, name, name, version, sha)
		case "/plugins/" + name + "/" + version + "/" + name + ".hpi":
			if hpiCalls != nil {
				*hpiCalls++
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(hpi)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func postPrefetchRaw(t *testing.T, baseURL, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/prefetch", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST prefetch: %v", err)
	}
	return resp
}

func prefetchBody(t *testing.T, plugins ...servabilityPlugin) []byte {
	t.Helper()
	b, err := json.Marshal(prefetchRequest{Plugins: plugins})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return b
}

func decodePrefetch(t *testing.T, resp *http.Response) prefetchResponse {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out prefetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode prefetch response: %v", err)
	}
	return out
}

func TestPrefetch_RequiresBearerToken(t *testing.T) {
	ts := newTestServer(t, nil, append(pullThroughWithResolver("http://127.0.0.1:1"), WithImportToken("correct-token"))...)
	body := prefetchBody(t, servabilityPlugin{Name: "a", Version: "1"})
	for _, token := range []string{"", "wrong"} {
		resp := postPrefetchRaw(t, ts.baseURL, token, body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, resp.StatusCode)
		}
	}
}

func TestPrefetch_PullThroughDisabled409(t *testing.T) {
	ts := newTestServer(t, nil, WithImportToken("correct-token"))
	resp := postPrefetchRaw(t, ts.baseURL, "correct-token", prefetchBody(t, servabilityPlugin{Name: "a", Version: "1"}))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func TestPrefetch_MethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, nil, WithImportToken("correct-token"))
	resp, err := http.Get(ts.baseURL + "/api/v1/prefetch")
	if err != nil {
		t.Fatalf("GET prefetch: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestPrefetch_RejectsTooManyEntries(t *testing.T) {
	ts := newTestServer(t, nil, append(pullThroughWithResolver("http://127.0.0.1:1"), WithImportToken("correct-token"))...)
	plugins := make([]servabilityPlugin, prefetchMaxEntries+1)
	for i := range plugins {
		plugins[i] = servabilityPlugin{Name: fmt.Sprintf("p%d", i), Version: "1"}
	}
	resp := postPrefetchRaw(t, ts.baseURL, "correct-token", prefetchBody(t, plugins...))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPrefetch_FetchesAndStores(t *testing.T) {
	const name, version = "pf-plugin", "1.5.0"
	hpi := newPluginBytes(name, version)
	var hpiCalls int
	upstream := prefetchUpstream(t, name, version, hpi, "", &hpiCalls)
	defer upstream.Close()

	ts := newTestServer(t, nil, append(pullThroughWithResolver(upstream.URL), WithImportToken("correct-token"))...)
	body := prefetchBody(t, servabilityPlugin{Name: name, Version: version})

	first := decodePrefetch(t, postPrefetchRaw(t, ts.baseURL, "correct-token", body))
	if len(first.Plugins) != 1 || first.Plugins[0].Outcome != prefetchOutcomeFetched {
		t.Fatalf("first answer = %+v, want fetched", first.Plugins)
	}
	if hpiCalls != 1 {
		t.Fatalf("upstream HPI calls = %d, want 1", hpiCalls)
	}

	second := decodePrefetch(t, postPrefetchRaw(t, ts.baseURL, "correct-token", body))
	if second.Plugins[0].Outcome != prefetchOutcomeStored {
		t.Fatalf("second answer = %+v, want stored", second.Plugins)
	}
	if hpiCalls != 1 {
		t.Fatalf("second prefetch hit upstream: HPI calls = %d, want 1", hpiCalls)
	}
}

func TestPrefetch_ResolvesLatest(t *testing.T) {
	const name, version = "pf-latest", "2.0.0"
	hpi := newPluginBytes(name, version)
	upstream := prefetchUpstream(t, name, version, hpi, "", nil)
	defer upstream.Close()

	ts := newTestServer(t, nil, append(pullThroughWithResolver(upstream.URL), WithImportToken("correct-token"))...)
	out := decodePrefetch(t, postPrefetchRaw(t, ts.baseURL, "correct-token",
		prefetchBody(t, servabilityPlugin{Name: name, Version: "latest"})))
	got := out.Plugins[0]
	if got.Outcome != prefetchOutcomeFetched || got.ResolvedVersion != version {
		t.Fatalf("answer = %+v, want fetched with resolvedVersion %s", got, version)
	}
}

func TestPrefetch_ChecksumMismatch(t *testing.T) {
	const name, version = "pf-badsum", "1.0.0"
	hpi := newPluginBytes(name, version)
	upstream := prefetchUpstream(t, name, version, hpi, sha256Base64([]byte("other bytes")), nil)
	defer upstream.Close()

	ts := newTestServer(t, nil, append(pullThroughWithResolver(upstream.URL), WithImportToken("correct-token"))...)
	out := decodePrefetch(t, postPrefetchRaw(t, ts.baseURL, "correct-token",
		prefetchBody(t, servabilityPlugin{Name: name, Version: version})))
	got := out.Plugins[0]
	if got.Outcome != prefetchOutcomeFailed || got.Reason != prefetchReasonChecksumMismatch {
		t.Fatalf("answer = %+v, want failed/checksum-mismatch", got)
	}
	resp, err := http.Get(ts.baseURL + "/api/v1/inventory")
	if err != nil {
		t.Fatalf("GET inventory: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if bytes.Contains(b, []byte(name)) {
		t.Fatalf("inventory lists %s after a checksum mismatch", name)
	}
}

func TestPrefetch_NotListed(t *testing.T) {
	upstream := prefetchUpstream(t, "pf-other", "1.0.0", newPluginBytes("pf-other", "1.0.0"), "", nil)
	defer upstream.Close()

	ts := newTestServer(t, nil, append(pullThroughWithResolver(upstream.URL), WithImportToken("correct-token"))...)
	out := decodePrefetch(t, postPrefetchRaw(t, ts.baseURL, "correct-token",
		prefetchBody(t, servabilityPlugin{Name: "pf-absent", Version: "1.0.0"})))
	got := out.Plugins[0]
	if got.Outcome != prefetchOutcomeFailed || got.Reason != prefetchReasonNotListed {
		t.Fatalf("answer = %+v, want failed/not-listed", got)
	}
}
