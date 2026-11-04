package ucmeta

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testTiers = Tiers{StableCores: []string{"2.580.1", "2.541.3", "2.568.3", "bad"}}

func TestTiersStableTierFor(t *testing.T) {
	tests := []struct {
		name  string
		tiers Tiers
		core  string
		want  string
		ok    bool
	}{
		{"non-tier patch maps to tier below", testTiers, "2.580.2", "2.580.1", true},
		{"tier maps to itself", testTiers, "2.580.1", "2.580.1", true},
		{"between tiers maps to lower tier", testTiers, "2.575.1", "2.568.3", true},
		{"above newest tier maps to newest", testTiers, "2.600.1", "2.580.1", true},
		{"below every tier maps to oldest", testTiers, "2.500.1", "2.541.3", true},
		{"latest is not a patch", testTiers, "latest", "", false},
		{"two segments is not a patch", testTiers, "2.580", "", false},
		{"empty core", testTiers, "", "", false},
		{"empty list", Tiers{}, "2.580.2", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.tiers.StableTierFor(tt.core)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("StableTierFor(%q) = (%q, %v), want (%q, %v)", tt.core, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestTiersSourceURL(t *testing.T) {
	const base = "https://updates.example"
	if got, want := testTiers.SourceURL(base, "2.580.2"), base+"/dynamic-stable-2.580.1/update-center.actual.json"; got != want {
		t.Fatalf("SourceURL with tiers = %q, want %q", got, want)
	}
	if got, want := (Tiers{}).SourceURL(base, "2.580.2"), base+"/dynamic-stable-2.580.2/update-center.actual.json"; got != want {
		t.Fatalf("SourceURL without tiers = %q, want %q", got, want)
	}
}

func TestFetchTiers(t *testing.T) {
	const doc = `{"stableCores":["2.516.3","2.580.1","2.580.2"],"weeklyCores":["2.588"]}`
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
		want    []string
	}{
		{
			name: "real-shaped document behind a redirect",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/tiers.json" {
					http.Redirect(w, r, "/final/tiers.json", http.StatusTemporaryRedirect)
					return
				}
				_, _ = io.WriteString(w, doc)
			},
			want: []string{"2.516.3", "2.580.1", "2.580.2"},
		},
		{
			name:    "not found",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			wantErr: true,
		},
		{
			name:    "bad json",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{") },
			wantErr: true,
		},
		{
			name:    "no stableCores key",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"plugins":{}}`) },
			wantErr: true,
		},
		{
			name:    "empty stableCores",
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"stableCores":[]}`) },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			got, err := FetchTiers(context.Background(), srv.Client(), srv.URL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("FetchTiers error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if strings.Join(got.StableCores, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("StableCores = %v, want %v", got.StableCores, tt.want)
			}
		})
	}
}

// tiersServer counts tiers.json reads and answers with the body the test sets.
type tiersServer struct {
	*httptest.Server
	mu     sync.Mutex
	hits   int
	status int
	body   string
}

func newTiersServer(status int, body string) *tiersServer {
	s := &tiersServer{status: status, body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.hits++
		if s.status != http.StatusOK {
			w.WriteHeader(s.status)
			return
		}
		_, _ = io.WriteString(w, s.body)
	}))
	return s
}

func (s *tiersServer) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *tiersServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

func testIndex(doer Doer, ttl time.Duration, now *time.Time) *TierIndex {
	x := NewTierIndex(doer, ttl, slog.New(slog.NewTextHandler(io.Discard, nil)))
	x.failureRetryBase = time.Millisecond
	x.now = func() time.Time { return *now }
	return x
}

func TestTierIndexCachesWithinTTL(t *testing.T) {
	srv := newTiersServer(http.StatusOK, `{"stableCores":["2.580.1"]}`)
	defer srv.Close()
	now := time.Unix(1000, 0)
	x := testIndex(srv.Client(), time.Hour, &now)

	for i := 0; i < 3; i++ {
		if got := x.SourceURL(context.Background(), srv.URL, "2.580.2"); !strings.Contains(got, "/dynamic-stable-2.580.1/") {
			t.Fatalf("SourceURL = %q, want the 2.580.1 tier", got)
		}
	}
	if n := srv.count(); n != 1 {
		t.Fatalf("tiers.json reads = %d, want 1 within the TTL", n)
	}

	now = now.Add(time.Hour + time.Second)
	x.SourceURL(context.Background(), srv.URL, "2.580.2")
	if n := srv.count(); n != 2 {
		t.Fatalf("tiers.json reads = %d, want 2 after the TTL", n)
	}
}

func TestTierIndexFailureKeepsLastGoodAndBacksOff(t *testing.T) {
	srv := newTiersServer(http.StatusOK, `{"stableCores":["2.580.1"]}`)
	defer srv.Close()
	now := time.Unix(1000, 0)
	x := testIndex(srv.Client(), time.Hour, &now)

	if _, ok := x.Lookup(context.Background(), srv.URL); !ok {
		t.Fatal("first Lookup ok = false, want true")
	}

	now = now.Add(time.Hour + time.Second)
	srv.setStatus(http.StatusInternalServerError)
	tiers, ok := x.Lookup(context.Background(), srv.URL)
	if !ok || len(tiers.StableCores) != 1 {
		t.Fatalf("Lookup after failure = (%v, %v), want the last good list", tiers, ok)
	}
	reads := srv.count()

	// Inside the backoff window the failed source is not read again.
	x.Lookup(context.Background(), srv.URL)
	if srv.count() != reads {
		t.Fatalf("tiers.json read inside the backoff window")
	}

	// After the backoff, a recovered source is read and used.
	now = now.Add(time.Second)
	srv.setStatus(http.StatusOK)
	if got := x.SourceURL(context.Background(), srv.URL, "2.580.2"); !strings.Contains(got, "/dynamic-stable-2.580.1/") {
		t.Fatalf("SourceURL after recovery = %q, want the 2.580.1 tier", got)
	}
}

func TestTierIndexFailureWithoutGoodListUsesCore(t *testing.T) {
	srv := newTiersServer(http.StatusInternalServerError, "")
	defer srv.Close()
	now := time.Unix(1000, 0)
	x := testIndex(srv.Client(), time.Hour, &now)

	if _, ok := x.Lookup(context.Background(), srv.URL); ok {
		t.Fatal("Lookup ok = true, want false when no list was ever read")
	}
	if got := x.SourceURL(context.Background(), srv.URL, "2.580.2"); !strings.Contains(got, "/dynamic-stable-2.580.2/") {
		t.Fatalf("SourceURL = %q, want the core-version URL", got)
	}
}

func TestTierIndexNilUsesCore(t *testing.T) {
	var x *TierIndex
	const base = "https://updates.example"
	if got, want := x.SourceURL(context.Background(), base, "2.580.2"), base+"/dynamic-stable-2.580.2/update-center.actual.json"; got != want {
		t.Fatalf("nil index SourceURL = %q, want %q", got, want)
	}
}
