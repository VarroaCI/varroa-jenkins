package wakeserver

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

type stubLister struct {
	controllers []*v1alpha1.Controller
}

func (s stubLister) ListControllers() []*v1alpha1.Controller { return s.controllers }

type stubWaker struct {
	hibernated []string
	nudges     []string
}

func (s *stubWaker) WakeHibernatedController(_ context.Context, namespace, name string) {
	s.hibernated = append(s.hibernated, namespace+"/"+name)
}

func (s *stubWaker) WakeController(cluster, namespace, name string) {
	s.nudges = append(s.nudges, cluster+":"+namespace+"/"+name)
}

func wakeController(name, namespace string, phase v1alpha1.ControllerPhase, powerState string) *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.ControllerSpec{PowerState: powerState},
		Status:     v1alpha1.ControllerStatus{Phase: phase},
	}
}

func TestServerMapsSubdomainAndPathRequests(t *testing.T) {
	subdomain := wakeController("ci", "team-a", v1alpha1.ControllerPhaseHibernated, "Hibernated")
	pathMode := wakeController("shared", "team-b", v1alpha1.ControllerPhaseRunning, "Running")
	pathMode.Spec.IngressSpec = &v1alpha1.IngressSpec{Mode: v1alpha1.RoutingModePath}
	lister := stubLister{controllers: []*v1alpha1.Controller{subdomain, pathMode}}

	t.Run("derived subdomain host and port", func(t *testing.T) {
		waker := &stubWaker{}
		srv := &Server{Lister: lister, Waker: waker, RootDomain: func() string { return "Example.COM" }}
		req := httptest.NewRequest(http.MethodGet, "http://CI.example.com:8080/job/example", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || len(waker.hibernated) != 1 || waker.hibernated[0] != "team-a/ci" {
			t.Fatalf("status=%d wake calls=%v", rec.Code, waker.hibernated)
		}
	})

	t.Run("path prefix", func(t *testing.T) {
		waker := &stubWaker{}
		srv := &Server{Lister: lister, Waker: waker, RootDomain: func() string { return "example.com" }}
		req := httptest.NewRequest(http.MethodGet, "http://dashboard.example.com/jenkins/team-b/shared/job/example", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || len(waker.hibernated) != 0 {
			t.Fatalf("status=%d wake calls=%v", rec.Code, waker.hibernated)
		}
	})

	t.Run("explicit host ResolveHost parity", func(t *testing.T) {
		explicit := wakeController("explicit", "team-c", v1alpha1.ControllerPhaseRunning, "Running")
		explicit.Spec.IngressSpec = &v1alpha1.IngressSpec{Host: "Custom.Example.net"}
		waker := &stubWaker{}
		srv := &Server{Lister: stubLister{controllers: []*v1alpha1.Controller{explicit}}, Waker: waker, RootDomain: func() string { return "ignored.example" }}
		req := httptest.NewRequest(http.MethodGet, "http://custom.example.NET/job/example", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d, want 503", rec.Code)
		}
	})

	t.Run("subdomain host falls back after path lookup miss", func(t *testing.T) {
		decoy := wakeController("y", "x", v1alpha1.ControllerPhaseHibernated, "Hibernated")
		waker := &stubWaker{}
		srv := &Server{Lister: stubLister{controllers: []*v1alpha1.Controller{decoy, subdomain}}, Waker: waker, RootDomain: func() string { return "example.com" }}
		req := httptest.NewRequest(http.MethodGet, "http://ci.example.com/jenkins/x/y", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || len(waker.hibernated) != 1 || waker.hibernated[0] != "team-a/ci" {
			t.Fatalf("status=%d wake calls=%v", rec.Code, waker.hibernated)
		}
	})
}

func TestServerRejectsEmptyOrUnresolvedHosts(t *testing.T) {
	pathMode := wakeController("shared", "team-b", v1alpha1.ControllerPhaseHibernated, "Hibernated")
	pathMode.Spec.IngressSpec = &v1alpha1.IngressSpec{Mode: v1alpha1.RoutingModePath}

	for _, host := range []string{"", ","} {
		t.Run("empty host "+host, func(t *testing.T) {
			waker := &stubWaker{}
			srv := &Server{Lister: stubLister{controllers: []*v1alpha1.Controller{pathMode}}, Waker: waker}
			req := httptest.NewRequest(http.MethodGet, "http://placeholder/", nil)
			req.Host = host
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound || len(waker.hibernated) != 0 || len(waker.nudges) != 0 {
				t.Fatalf("status=%d CAS=%v nudges=%v", rec.Code, waker.hibernated, waker.nudges)
			}
		})
	}

	waker := &stubWaker{}
	srv := &Server{Lister: stubLister{controllers: []*v1alpha1.Controller{pathMode}}, Waker: waker, RootDomain: func() string { return "example.com" }}
	rec := serve(t, srv, "http://shared.example.com/", "application/json")
	if rec.Code != http.StatusNotFound || len(waker.hibernated) != 0 {
		t.Fatalf("path-mode host match status=%d CAS=%v, want 404/no wake", rec.Code, waker.hibernated)
	}
}

func TestServerWakeDispatch(t *testing.T) {
	t.Run("hibernated CAS exactly once", func(t *testing.T) {
		cr := wakeController("ci", "team-a", v1alpha1.ControllerPhaseHibernated, "Hibernated")
		waker := &stubWaker{}
		srv := &Server{Lister: stubLister{[]*v1alpha1.Controller{cr}}, Waker: waker, RootDomain: func() string { return "example.com" }}
		rec := serve(t, srv, "http://ci.example.com/", "application/json")
		if rec.Code != http.StatusServiceUnavailable || len(waker.hibernated) != 1 || len(waker.nudges) != 0 {
			t.Fatalf("status=%d CAS=%v nudges=%v", rec.Code, waker.hibernated, waker.nudges)
		}
	})

	t.Run("stopped never wakes and always returns JSON", func(t *testing.T) {
		cr := wakeController("ci", "team-a", v1alpha1.ControllerPhaseStopped, "Stopped")
		waker := &stubWaker{}
		srv := &Server{Lister: stubLister{[]*v1alpha1.Controller{cr}}, Waker: waker, RootDomain: func() string { return "example.com" }}
		rec := serve(t, srv, "http://ci.example.com/", "text/html")
		if rec.Code != http.StatusServiceUnavailable || len(waker.hibernated) != 0 || len(waker.nudges) != 0 {
			t.Fatalf("status=%d CAS=%v nudges=%v", rec.Code, waker.hibernated, waker.nudges)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type=%q, want JSON", got)
		}
	})

	t.Run("connected nudges owning shard", func(t *testing.T) {
		cr := wakeController("ci", "team-a", v1alpha1.ControllerPhaseConnected, "Running")
		waker := &stubWaker{}
		srv := &Server{Lister: stubLister{[]*v1alpha1.Controller{cr}}, Waker: waker, RootDomain: func() string { return "example.com" }}
		serve(t, srv, "http://ci.example.com/", "text/html")
		if len(waker.nudges) != 1 || waker.nudges[0] != ":team-a/ci" {
			t.Fatalf("nudges=%v, want empty-cluster nudge", waker.nudges)
		}
	})
}

func TestServerContentNegotiation(t *testing.T) {
	cr := wakeController("ci", "team-a", v1alpha1.ControllerPhaseProvisioning, "Running")
	srv := &Server{Lister: stubLister{[]*v1alpha1.Controller{cr}}, Waker: &stubWaker{}, RootDomain: func() string { return "example.com" }}

	html := serve(t, srv, "http://ci.example.com/job/example?x=1", "text/html,application/xhtml+xml")
	if html.Code != http.StatusServiceUnavailable || html.Header().Get("Retry-After") != "5" {
		t.Fatalf("HTML status=%d Retry-After=%q", html.Code, html.Header().Get("Retry-After"))
	}
	for _, want := range []string{"Waking Controller", `var statusPath = "/.varroa/wake/status";`, `var targetURL = "/job/example?x=1";`, "var redirectOnNonWake = true;"} {
		if !strings.Contains(html.Body.String(), want) {
			t.Errorf("HTML missing %q", want)
		}
	}

	jsonResponse := serve(t, srv, "http://ci.example.com/job/example", "application/json")
	if jsonResponse.Code != http.StatusServiceUnavailable || jsonResponse.Header().Get("Retry-After") != "5" {
		t.Fatalf("JSON status=%d Retry-After=%q", jsonResponse.Code, jsonResponse.Header().Get("Retry-After"))
	}
	var body map[string]interface{}
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &body); err != nil || body["error"] != "controller is waking" {
		t.Fatalf("JSON body=%q error=%v", jsonResponse.Body.String(), err)
	}
}

func TestServerStatusContract(t *testing.T) {
	cr := wakeController("ci", "team-a", v1alpha1.ControllerPhaseRunning, "Running")
	cr.Spec.IngressSpec = &v1alpha1.IngressSpec{Mode: v1alpha1.RoutingModePath}
	waker := &stubWaker{}
	srv := &Server{Lister: stubLister{[]*v1alpha1.Controller{cr}}, Waker: waker, RootDomain: func() string { return "example.com" }}
	rec := serve(t, srv, "http://dashboard.example.com/jenkins/team-a/ci/.varroa/wake/status", "application/json")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d Cache-Control=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["varroaWake"] != true || body["phase"] != "Running" {
		t.Fatalf("body=%q error=%v", rec.Body.String(), err)
	}
	if len(waker.hibernated) != 0 || len(waker.nudges) != 0 {
		t.Fatalf("status request woke controller: CAS=%v nudges=%v", waker.hibernated, waker.nudges)
	}
}

func TestServerUnknownHostIsMarkerless(t *testing.T) {
	cr := wakeController("ci", "team-a", v1alpha1.ControllerPhaseHibernated, "Hibernated")
	waker := &stubWaker{}
	srv := &Server{Lister: stubLister{[]*v1alpha1.Controller{cr}}, Waker: waker, RootDomain: func() string { return "example.com" }}
	rec := serve(t, srv, "http://unknown.example.com/", "application/json")
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "varroaWake") {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(waker.hibernated) != 0 || len(waker.nudges) != 0 {
		t.Fatalf("unknown host woke controller: CAS=%v nudges=%v", waker.hibernated, waker.nudges)
	}
}

func TestServerLogsWakeNudgeAndUnknownController(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hibernated := wakeController("sleeping", "team-a", v1alpha1.ControllerPhaseHibernated, "Hibernated")
	connected := wakeController("ready", "team-b", v1alpha1.ControllerPhaseConnected, "Running")
	waker := &stubWaker{}
	srv := &Server{
		Lister:     stubLister{controllers: []*v1alpha1.Controller{hibernated, connected}},
		Waker:      waker,
		RootDomain: func() string { return "example.com" },
		Logger:     logger,
	}

	serve(t, srv, "http://sleeping.example.com/job/a", "application/json")
	serve(t, srv, "http://ready.example.com/job/b", "application/json")
	serve(t, srv, "http://unknown.example.com/job/c", "application/json")

	for _, want := range []string{
		"woke hibernated controller from navigation",
		"controller=team-a/sleeping",
		"nudged connected controller to remove stale wake slice",
		"controller=team-b/ready",
		"wake request did not match a controller",
		"host=unknown.example.com",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

func serve(t *testing.T, srv *Server, target, accept string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Accept", accept)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}
