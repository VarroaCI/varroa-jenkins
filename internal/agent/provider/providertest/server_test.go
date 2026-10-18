package providertest

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecordsHeadersAndBodiesInOrder asserts that every request is recorded
// with its headers and body, in arrival order.
func TestRecordsHeadersAndBodiesInOrder(t *testing.T) {
	t.Parallel()
	fake := New(Script{Turns: []Turn{{Text: "one"}, {Text: "two"}}})
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)

	for i, body := range []string{`{"n":1}`, `{"n":2}`} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("x-api-key", "k"+string(rune('0'+i)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(reqs))
	}
	if !bytes.Equal(reqs[0].Body, []byte(`{"n":1}`)) || !bytes.Equal(reqs[1].Body, []byte(`{"n":2}`)) {
		t.Fatalf("bodies out of order: %q, %q", reqs[0].Body, reqs[1].Body)
	}
	if reqs[0].Header.Get("x-api-key") != "k0" || reqs[1].Header.Get("x-api-key") != "k1" {
		t.Fatal("headers were not recorded per request")
	}
	if reqs[0].Path != "/v1/messages" {
		t.Fatalf("path = %s", reqs[0].Path)
	}
}

// TestUnknownPathIsNotFound asserts that other paths answer 404 and record nothing.
func TestUnknownPathIsNotFound(t *testing.T) {
	t.Parallel()
	fake := New(Script{})
	rec := httptest.NewRecorder()
	fake.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if len(fake.Requests()) != 0 {
		t.Fatal("a 404 request was recorded")
	}
}
