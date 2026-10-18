package forge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDialControl_RefusesLocalAddresses(t *testing.T) {
	refused := []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:443", "[fe80::1]:443", "0.0.0.0:443", "[::]:443"}
	for _, a := range refused {
		if err := dialControl("tcp", a, nil); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: err %v, want refused", a, err)
		}
	}
	for _, a := range []string{"10.0.0.5:443", "93.184.216.34:443"} {
		if err := dialControl("tcp", a, nil); err != nil {
			t.Errorf("%s: refused: %v", a, err)
		}
	}
}

func TestNewHTTPClient_RefusesLoopbackServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := NewHTTPClient().Do(req)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("loopback dial: err %v, want refused", err)
	}
}

func TestNewHTTPClient_RefusesRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := NewHTTPClient()
	// The test server dials 127.0.0.1, which the dialer refuses. Use the test
	// server's transport so that only the redirect rule is under test.
	c.Transport = srv.Client().Transport
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	_, err := c.Do(req)
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("err %v, want ErrRedirect", err)
	}
}

func TestNewHTTPClient_Timeout(t *testing.T) {
	if got := NewHTTPClient().Timeout; got != 15*time.Second {
		t.Fatalf("timeout %v", got)
	}
}
