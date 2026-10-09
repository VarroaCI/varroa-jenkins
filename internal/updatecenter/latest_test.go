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

// latestUpstream serves one upstream update-center listing of name at version, plus
// its HPI bytes, the way the weekly update center does.
func latestUpstream(t *testing.T, name, version string, hpi []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/update-center.actual.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"updateCenterVersion":"1","plugins":{%q:{"name":%q,"version":%q,"sha256":%q,"dependencies":[]}}}`,
				name, name, version, sha256Base64(hpi))
		case "/plugins/" + name + "/" + version + "/" + name + ".hpi":
			_, _ = w.Write(hpi)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getBody(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp, body
}

func TestDownload_LatestServesAdvertisedStoredVersion(t *testing.T) {
	store := newTestStore(t)
	oldBytes := newPluginBytes("lat-plugin", "1.0")
	newerBytes := newPluginBytes("lat-plugin", "2.0")
	seedPluginPack(t, store, "pack:lat-v1", []testPlugin{{name: "lat-plugin", version: "1.0", hpiBytes: oldBytes}})
	seedPluginPack(t, store, "pack:lat-v2", []testPlugin{{name: "lat-plugin", version: "2.0", hpiBytes: newerBytes}})
	ts := newTestServer(t, store)

	_, metaBody := getBody(t, ts.baseURL+"/update-center.actual.json")
	var meta updateCenterJSON
	if err := json.Unmarshal(metaBody, &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	adv, ok := meta.Plugins["lat-plugin"]
	if !ok || adv.Version != "2.0" {
		t.Fatalf("metadata advertises %+v, want version 2.0", adv)
	}

	resp, body := getBody(t, ts.baseURL+"/download/plugins/lat-plugin/latest/lat-plugin.hpi")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, newerBytes) {
		t.Errorf("body is not the advertised 2.0 bytes")
	}
	if got, want := resp.Header.Get("sha256"), sha256Hex(newerBytes); got != want {
		t.Errorf("sha256 header = %s, want %s", got, want)
	}
}

func TestDownload_LatestPullThroughStoresConcreteVersion(t *testing.T) {
	store := newTestStore(t)
	hpi := newPluginBytes("lat-pt", "3.0")
	upstream := latestUpstream(t, "lat-pt", "3.0", hpi)
	ts := newTestServer(t, store, pullThroughWithResolver(upstream.URL)...)

	resp, body := getBody(t, ts.baseURL+"/download/plugins/lat-pt/latest/lat-pt.hpi")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(body, hpi) {
		t.Errorf("body mismatch")
	}

	_, invBody := getBody(t, ts.baseURL+"/api/v1/inventory")
	var inv inventoryResponse
	if err := json.Unmarshal(invBody, &inv); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	stored := false
	for _, e := range inv.Plugins {
		if e.Version == "latest" {
			t.Errorf("store holds version latest: %+v", e)
		}
		if e.Name == "lat-pt" && e.Version == "3.0" {
			stored = true
		}
	}
	if !stored {
		t.Errorf("inventory does not hold lat-pt@3.0: %+v", inv.Plugins)
	}
}

func TestDownload_LatestPullThroughDisabled404(t *testing.T) {
	ts := newTestServer(t, newTestStore(t))
	resp, _ := getBody(t, ts.baseURL+"/download/plugins/lat-none/latest/lat-none.hpi")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestDownload_LatestUnlisted404(t *testing.T) {
	upstream := latestUpstream(t, "other-plugin", "1.0", newPluginBytes("other-plugin", "1.0"))
	ts := newTestServer(t, newTestStore(t), pullThroughWithResolver(upstream.URL)...)
	resp, _ := getBody(t, ts.baseURL+"/download/plugins/ghost-plugin/latest/ghost-plugin.hpi")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
