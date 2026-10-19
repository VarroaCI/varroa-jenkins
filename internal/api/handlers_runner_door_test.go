package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitesrv "github.com/varroaci/varroa-jenkins/internal/mite"
)

// doorFakeBrood is a cluster-aware Brood stub. Unlike the single-cluster
// fakeBrood in contract_test.go, it keeps a separate Controller set per
// cluster, so a test can prove the door resolves a same-named cell on the
// cluster placement.cluster names, not just any cluster (task 8.3a).
type doorFakeBrood struct {
	local       string
	controllers map[string]map[string]*v1alpha1.Controller // cluster -> "ns/name" -> CR
}

func newDoorFakeBrood() *doorFakeBrood {
	return &doorFakeBrood{local: "core", controllers: make(map[string]map[string]*v1alpha1.Controller)}
}

func (b *doorFakeBrood) seed(cluster string, cr *v1alpha1.Controller) {
	if b.controllers[cluster] == nil {
		b.controllers[cluster] = make(map[string]*v1alpha1.Controller)
	}
	b.controllers[cluster][cr.Namespace+"/"+cr.Name] = cr
}

func (b *doorFakeBrood) LocalCluster() string { return b.local }

func (b *doorFakeBrood) Clusters(_ context.Context) ([]ClusterInfo, error) { return nil, nil }

func (b *doorFakeBrood) IsKnown(_ context.Context, _ string) bool { return true }

func (b *doorFakeBrood) ListAll(_ context.Context, _, _ string) ([]ClusterController, []ClusterFanoutStatus, error) {
	return nil, nil, nil
}

func (b *doorFakeBrood) Get(_ context.Context, cluster, ns, name string) (*v1alpha1.Controller, error) {
	m := b.controllers[cluster]
	if m == nil {
		return nil, fmt.Errorf("doorFakeBrood: cluster %q not known", cluster)
	}
	cr, ok := m[ns+"/"+name]
	if !ok {
		return nil, fmt.Errorf("doorFakeBrood: controller %s/%s not found on %s", ns, name, cluster)
	}
	return cr, nil
}

func (b *doorFakeBrood) Create(_ context.Context, _ string, _ ControllersCreateArgs) (*v1alpha1.Controller, []bus.Check, error) {
	return nil, nil, fmt.Errorf("doorFakeBrood: Create not implemented")
}

func (b *doorFakeBrood) Preflight(_ context.Context, _ string, _ ControllersCreateArgs) ([]bus.Check, error) {
	return nil, fmt.Errorf("doorFakeBrood: Preflight not implemented")
}

func (b *doorFakeBrood) Update(_ context.Context, _, _, _ string, _ json.RawMessage, _ string, _ bool) (*v1alpha1.Controller, []bus.Check, []bus.UnappliedRemoval, error) {
	return nil, nil, nil, fmt.Errorf("doorFakeBrood: Update not implemented")
}

func (b *doorFakeBrood) Delete(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("doorFakeBrood: Delete not implemented")
}

func (b *doorFakeBrood) DeletePod(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("doorFakeBrood: DeletePod not implemented")
}

func (b *doorFakeBrood) Drain(_ context.Context, _, _ string) (string, error) {
	return "", fmt.Errorf("doorFakeBrood: Drain not implemented")
}

func (b *doorFakeBrood) DrainCancel(_ context.Context, _, _ string) (string, error) {
	return "", fmt.Errorf("doorFakeBrood: DrainCancel not implemented")
}

func (b *doorFakeBrood) StateOf(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("doorFakeBrood: StateOf not implemented")
}

func (b *doorFakeBrood) DiscoverNamespaces(_ context.Context, _ string) (*bus.NamespacesListResponse, error) {
	return nil, fmt.Errorf("doorFakeBrood: DiscoverNamespaces not implemented")
}

// newDoorTestServer builds a Server wired for the agent door: an empty
// crdstore.Fake, an empty RunnerTokenIndex, brood, and a real
// MiteTokenSigner (the door mints a real operator Jenkins token on every
// action past the login check).
func newDoorTestServer(t *testing.T, brood Brood) (*Server, *crdstore.Fake, *RunnerTokenIndex) {
	t.Helper()
	store := crdstore.NewFake()
	idx := NewRunnerTokenIndex()
	signer, err := mitesrv.NewMiteTokenSigner()
	if err != nil {
		t.Fatalf("NewMiteTokenSigner: %v", err)
	}
	srv := NewServer(&Dependencies{
		Store:              store,
		Logger:             slog.Default(),
		Brood:              brood,
		RunnerTokenIndex:   idx,
		JenkinsTokenSigner: signer,
	})
	return srv, store, idx
}

func seedDoorTeam(t *testing.T, store *crdstore.Fake, placement *v1alpha1.TeamPlacementStatus) {
	t.Helper()
	crdstore.MustSeed(store, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Status:     v1alpha1.TeamStatus{Placement: placement},
	})
}

func seedDoorRunner(t *testing.T, store *crdstore.Fake, idx *RunnerTokenIndex, r *v1alpha1.Runner) {
	t.Helper()
	crdstore.MustSeed(store, r)
	idx.Put(r.Name, r.Status.Token)
}

// --- 8.3: path shapes, token causes, ssh refusal, empty placement ---

func TestRunnerDoor_UnknownPathShape404(t *testing.T) {
	srv, _, _ := newDoorTestServer(t, newDoorFakeBrood())
	paths := []string{
		"/runners/tok",
		"/runners/tok/",
		"/runners/tok/scriptText",
		"/runners/tok/wsagents",
		"/runners/tok/jnlpJars/other.jar",
		"/runners/",
	}
	for _, path := range paths {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.HandleRunnerDoor(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestRunnerDoor_NonGETMethod404(t *testing.T) {
	srv, _, _ := newDoorTestServer(t, newDoorFakeBrood())
	req := httptest.NewRequest(http.MethodPost, "/runners/tok/login", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRunnerDoor_UnknownTokenUnauthorized(t *testing.T) {
	srv, _, _ := newDoorTestServer(t, newDoorFakeBrood())
	req := httptest.NewRequest(http.MethodGet, "/runners/does-not-exist/login", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Body.String() != `{"error":"unauthorized"}` {
		t.Fatalf("body = %q, want the standard unauthorized body", rec.Body.String())
	}
}

// TestRunnerDoor_EmptyStoredTokenUnauthorized covers an index entry that
// outlives the Runner's own cleared token — the live CRD is always the
// final check, never the index alone.
func TestRunnerDoor_EmptyStoredTokenUnauthorized(t *testing.T) {
	srv, store, idx := newDoorTestServer(t, newDoorFakeBrood())
	crdstore.MustSeed(store, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/home/jenkins"},
		Status:     v1alpha1.RunnerStatus{Token: ""},
	})
	idx.Put("r1", "stale-token")

	req := httptest.NewRequest(http.MethodGet, "/runners/stale-token/login", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRunnerDoor_SSHRunnerRefused(t *testing.T) {
	srv, store, idx := newDoorTestServer(t, newDoorFakeBrood())
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec: v1alpha1.RunnerSpec{
			Team: "payments", Launcher: "ssh", RemoteFS: "/x",
			Host: "1.2.3.4", CredentialRef: "cred",
		},
		Status: v1alpha1.RunnerStatus{Token: "tok"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/login", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRunnerDoor_NilPlacementServiceUnavailable(t *testing.T) {
	srv, store, idx := newDoorTestServer(t, newDoorFakeBrood())
	crdstore.MustSeed(store, &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "payments"}})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok", NodeName: "payments-r1"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/login", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestRunnerDoor_EmptyPlacementClusterServiceUnavailable pins task 8.3a:
// an empty placement.cluster answers 503 even when cell and cellNamespace
// are both set.
func TestRunnerDoor_EmptyPlacementClusterServiceUnavailable(t *testing.T) {
	srv, store, idx := newDoorTestServer(t, newDoorFakeBrood())
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok", NodeName: "payments-r1"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/login", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestRunnerDoor_ResolvesCellByPlacementCluster pins task 8.3a: two
// clusters each hold a Controller named "cell1" in namespace "team-ns".
// The door must resolve the one on placement.cluster ("hive-b"), never
// the local cluster's same-named cell.
func TestRunnerDoor_ResolvesCellByPlacementCluster(t *testing.T) {
	corePayload := []byte("core-cluster-cell-jar")
	devPayload := []byte("hive-b-cluster-cell-jar")
	coreSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(corePayload)
	}))
	defer coreSrv.Close()
	devSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(devPayload)
	}))
	defer devSrv.Close()

	brood := newDoorFakeBrood()
	coreCell := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "team-ns"}}
	devCell := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "team-ns"}}
	brood.seed("core", coreCell)
	brood.seed("hive-b", devCell)

	srv, store, idx := newDoorTestServer(t, brood)
	srv.runnerDoorServiceURL = func(cr *v1alpha1.Controller) string {
		if cr == devCell {
			return devSrv.URL
		}
		return coreSrv.URL
	}
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "hive-b", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok", NodeName: "payments-r1"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/jnlpJars/agent.jar", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), devPayload) {
		t.Fatalf("body = %q, want the hive-b cluster's cell payload %q (door resolved the wrong cluster)",
			rec.Body.Bytes(), devPayload)
	}
}

// --- 8.4: login ---

func TestRunnerDoor_LoginSucceeds(t *testing.T) {
	srv, store, idx := newDoorTestServer(t, newDoorFakeBrood())
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/login", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "{\"varroaRunnerDoor\":true}\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// --- 8.5: the agent-secret fetch ---

func TestFetchRunnerAgentSecret_ValidBody(t *testing.T) {
	var gotAuth string
	jnlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`<jnlp><application-desc><argument>abc123secret</argument><argument>other</argument></application-desc></jnlp>`))
	}))
	defer jnlpSrv.Close()

	srv, _, _ := newDoorTestServer(t, newDoorFakeBrood())
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return jnlpSrv.URL }
	cell := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"}}

	secret, err := srv.fetchRunnerAgentSecret(context.Background(), cell, "payments-r1")
	if err != nil {
		t.Fatalf("fetchRunnerAgentSecret: %v", err)
	}
	if secret != "abc123secret" {
		t.Fatalf("secret = %q, want %q", secret, "abc123secret")
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("Authorization = %q, want a Bearer token", gotAuth)
	}
}

func TestFetchRunnerAgentSecret_Forbidden(t *testing.T) {
	jnlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer jnlpSrv.Close()

	srv, _, _ := newDoorTestServer(t, newDoorFakeBrood())
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return jnlpSrv.URL }
	cell := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"}}

	if _, err := srv.fetchRunnerAgentSecret(context.Background(), cell, "payments-r1"); err == nil {
		t.Fatal("expected an error for a 403 response")
	}
}

func TestFetchRunnerAgentSecret_NoArgumentElement(t *testing.T) {
	jnlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<jnlp><application-desc></application-desc></jnlp>`))
	}))
	defer jnlpSrv.Close()

	srv, _, _ := newDoorTestServer(t, newDoorFakeBrood())
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return jnlpSrv.URL }
	cell := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"}}

	if _, err := srv.fetchRunnerAgentSecret(context.Background(), cell, "payments-r1"); err == nil {
		t.Fatal("expected an error for a body with no argument element")
	}
}

// TestFetchRunnerAgentSecret_EmptyArgumentElement proves an empty or
// whitespace-only <argument> is a fetch failure, never a forwarded empty
// Secret-Key: the door SHALL fetch a real secret on every connection
// attempt, and an empty string is not one.
func TestFetchRunnerAgentSecret_EmptyArgumentElement(t *testing.T) {
	jnlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<jnlp><application-desc><argument>   </argument></application-desc></jnlp>`))
	}))
	defer jnlpSrv.Close()

	srv, _, _ := newDoorTestServer(t, newDoorFakeBrood())
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return jnlpSrv.URL }
	cell := &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "ns"}}

	if _, err := srv.fetchRunnerAgentSecret(context.Background(), cell, "payments-r1"); err == nil {
		t.Fatal("expected an error for a body with an empty argument element")
	}
}

// TestRunnerDoorUpgrade_EmptySecretAnswers503 proves the upgrade handler
// itself turns an empty agent secret into a 503, rather than proxying the
// WebSocket upgrade with an empty Secret-Key header.
func TestRunnerDoorUpgrade_EmptySecretAnswers503(t *testing.T) {
	jnlpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "jenkins-agent.jnlp") {
			_, _ = w.Write([]byte(`<jnlp><application-desc><argument></argument></application-desc></jnlp>`))
			return
		}
		http.NotFound(w, r)
	}))
	defer jnlpSrv.Close()

	brood := newDoorFakeBrood()
	brood.seed("core", &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "team-ns"}})
	srv, store, idx := newDoorTestServer(t, brood)
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return jnlpSrv.URL }
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok", NodeName: "payments-r1"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/wsagents/", nil)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for an empty agent secret: %s", rec.Code, rec.Body.String())
	}
}

// --- 8.7: the agent.jar stream ---

func TestRunnerDoorAgentJar_StreamsBody(t *testing.T) {
	payload := bytes.Repeat([]byte{0xCA, 0xFE}, 1000)
	cellSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jnlpJars/agent.jar" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing bearer", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer cellSrv.Close()

	brood := newDoorFakeBrood()
	brood.seed("core", &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "team-ns"}})
	srv, store, idx := newDoorTestServer(t, brood)
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return cellSrv.URL }
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok", NodeName: "payments-r1"},
	})

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/jnlpJars/agent.jar", nil)
	rec := httptest.NewRecorder()
	srv.HandleRunnerDoor(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != len(payload) {
		t.Fatalf("content length = %d, want %d", rec.Body.Len(), len(payload))
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatal("streamed body does not match the source payload")
	}
}

// --- 8.6: the upgrade proxy ---

// TestRunnerDoorUpgrade_HeaderContract drives a real WebSocket-style
// upgrade through httptest.NewServer (needed for http.Hijacker support,
// which httptest.ResponseRecorder does not implement) and asserts every
// row of specs/agent-websocket-door/spec.md's header table.
func TestRunnerDoorUpgrade_HeaderContract(t *testing.T) {
	const wantSecret = "wsagent-secret-xyz"
	const wantNodeName = "payments-r1"

	var mu sync.Mutex
	var cellHeaders http.Header
	cellSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/jenkins-agent.jnlp"):
			_, _ = w.Write([]byte(`<jnlp><application-desc><argument>` + wantSecret + `</argument></application-desc></jnlp>`))
		case r.URL.Path == "/wsagents/":
			mu.Lock()
			cellHeaders = r.Header.Clone()
			mu.Unlock()
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "response writer does not support hijacking", http.StatusInternalServerError)
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			resp := "HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\n" +
				"Connection: Upgrade\r\n" +
				"Sec-WebSocket-Accept: fake-accept-value\r\n" +
				"X-Remoting-Capability: 3\r\n" +
				"X-Remoting-Minimum-Version: 3.17\r\n" +
				"Connection-Cookie: cookie-from-cell\r\n" +
				"X-Extra-Unnamed: should-be-dropped\r\n" +
				"\r\n"
			_, _ = buf.WriteString(resp)
			_ = buf.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	defer cellSrv.Close()

	brood := newDoorFakeBrood()
	brood.seed("core", &v1alpha1.Controller{ObjectMeta: metav1.ObjectMeta{Name: "cell1", Namespace: "team-ns"}})
	srv, store, idx := newDoorTestServer(t, brood)
	srv.runnerDoorServiceURL = func(*v1alpha1.Controller) string { return cellSrv.URL }
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok", NodeName: wantNodeName},
	})

	doorSrv := httptest.NewServer(http.HandlerFunc(srv.HandleRunnerDoor))
	defer doorSrv.Close()
	doorURL, err := url.Parse(doorSrv.URL)
	if err != nil {
		t.Fatalf("parse door URL: %v", err)
	}

	conn, err := net.Dial("tcp", doorURL.Host)
	if err != nil {
		t.Fatalf("dial door: %v", err)
	}
	defer func() { _ = conn.Close() }()

	reqLines := []string{
		"GET /runners/tok/wsagents/ HTTP/1.1",
		"Host: " + doorURL.Host,
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==",
		"Sec-WebSocket-Version: 13",
		"Sec-WebSocket-Protocol: remoting",
		"Node-Name: attacker-node",
		"Secret-Key: guessed-secret",
		"X-Remoting-Capability: 3",
		"Connection-Cookie: client-cookie-value",
		"Cookie: session=evil",
		"", "",
	}
	if _, err := conn.Write([]byte(strings.Join(reqLines, "\r\n"))); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}

	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("status line = %q, want a 101 response", statusLine)
	}
	respHeaders := make(http.Header)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read response headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		respHeaders.Add(strings.TrimSpace(k), strings.TrimSpace(v))
	}

	// Response-side handshake and remoting headers reach the client
	// unchanged; the unnamed header does not.
	if got := respHeaders.Get("Sec-WebSocket-Accept"); got != "fake-accept-value" {
		t.Errorf("Sec-WebSocket-Accept = %q, want %q", got, "fake-accept-value")
	}
	if got := respHeaders.Get("X-Remoting-Capability"); got != "3" {
		t.Errorf("X-Remoting-Capability = %q, want %q", got, "3")
	}
	if got := respHeaders.Get("X-Remoting-Minimum-Version"); got != "3.17" {
		t.Errorf("X-Remoting-Minimum-Version = %q, want %q", got, "3.17")
	}
	if got := respHeaders.Get("Connection-Cookie"); got != "cookie-from-cell" {
		t.Errorf("Connection-Cookie = %q, want %q", got, "cookie-from-cell")
	}
	if got := respHeaders.Get("X-Extra-Unnamed"); got != "" {
		t.Errorf("X-Extra-Unnamed = %q, want it dropped", got)
	}

	// Request-side: the cell saw the door's own Node-Name and the fetched
	// secret, never the client's values; the handshake headers and the two
	// named remoting headers reached the cell unchanged; Cookie did not.
	mu.Lock()
	seen := cellHeaders
	mu.Unlock()
	if seen == nil {
		t.Fatal("the cell never received the upgrade request")
	}
	if got := seen.Get("Node-Name"); got != wantNodeName {
		t.Errorf("cell saw Node-Name = %q, want %q (the client's attacker-node must be discarded)", got, wantNodeName)
	}
	if got := seen.Get("Secret-Key"); got != wantSecret {
		t.Errorf("cell saw Secret-Key = %q, want the fetched secret %q", got, wantSecret)
	}
	if got := seen.Get("Sec-WebSocket-Key"); got != "dGhlIHNhbXBsZSBub25jZQ==" {
		t.Errorf("cell saw Sec-WebSocket-Key = %q, want it relayed unchanged", got)
	}
	if got := seen.Get("Sec-WebSocket-Version"); got != "13" {
		t.Errorf("cell saw Sec-WebSocket-Version = %q, want it relayed unchanged", got)
	}
	if got := seen.Get("Sec-WebSocket-Protocol"); got != "remoting" {
		t.Errorf("cell saw Sec-WebSocket-Protocol = %q, want it relayed unchanged", got)
	}
	if got := seen.Get("X-Remoting-Capability"); got != "3" {
		t.Errorf("cell saw X-Remoting-Capability = %q, want it copied upstream", got)
	}
	if got := seen.Get("Connection-Cookie"); got != "client-cookie-value" {
		t.Errorf("cell saw Connection-Cookie = %q, want it copied upstream", got)
	}
	if got := seen.Get("Cookie"); got != "" {
		t.Errorf("cell saw Cookie = %q, want it dropped", got)
	}
}

// --- 8.2: the token index ---

func TestRunnerTokenIndex_RotationReplacesEntry(t *testing.T) {
	idx := NewRunnerTokenIndex()
	idx.Put("r1", "old-token")
	if name, ok := idx.Resolve("old-token"); !ok || name != "r1" {
		t.Fatalf("resolve old token: name=%q ok=%v", name, ok)
	}

	idx.Put("r1", "new-token")
	if _, ok := idx.Resolve("old-token"); ok {
		t.Fatal("the old token still resolves after rotation")
	}
	if name, ok := idx.Resolve("new-token"); !ok || name != "r1" {
		t.Fatalf("resolve new token: name=%q ok=%v", name, ok)
	}
}

func TestRunnerTokenIndex_RemoveDropsEntry(t *testing.T) {
	idx := NewRunnerTokenIndex()
	idx.Put("r1", "tok")
	idx.Remove("r1")
	if _, ok := idx.Resolve("tok"); ok {
		t.Fatal("the token still resolves after Remove")
	}
}

func TestRunnerTokenIndex_EmptyTokenNeverIndexed(t *testing.T) {
	idx := NewRunnerTokenIndex()
	idx.Put("r1", "")
	if _, ok := idx.Resolve(""); ok {
		t.Fatal("an empty token must never resolve")
	}
}

// --- 8.8: the door bypasses auth middleware ---

// denyAllProvider is an auth.Provider stub that refuses every token. It
// proves TestRunnerDoor_BypassesAuthMiddleware never actually reaches it.
type denyAllProvider struct{}

func (denyAllProvider) Mode() auth.AuthMode { return auth.AuthMode("test") }

func (denyAllProvider) Verify(_ context.Context, _ string) (*auth.Claims, error) {
	return nil, fmt.Errorf("denied")
}

func (denyAllProvider) CookieDomain() string { return "" }

func (denyAllProvider) Discovery() ([]byte, []byte, bool) { return nil, nil, false }

func TestRunnerDoor_BypassesAuthMiddleware(t *testing.T) {
	srv, store, idx := newDoorTestServer(t, newDoorFakeBrood())
	seedDoorTeam(t, store, &v1alpha1.TeamPlacementStatus{Cluster: "core", Cell: "cell1", CellNamespace: "team-ns"})
	seedDoorRunner(t, store, idx, &v1alpha1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "r1"},
		Spec:       v1alpha1.RunnerSpec{Team: "payments", Launcher: "inbound", RemoteFS: "/x"},
		Status:     v1alpha1.RunnerStatus{Token: "tok"},
	})

	mux := http.NewServeMux()
	mux.HandleFunc(runnerDoorPrefix, srv.HandleRunnerDoor)
	handler := auth.AuthMiddleware(denyAllProvider{}, nil, nil, nil, nil, mux, slog.Default())

	req := httptest.NewRequest(http.MethodGet, "/runners/tok/login", nil) // no Authorization header
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (AuthMiddleware must bypass %s): %s", rec.Code, runnerDoorPrefix, rec.Body.String())
	}
}
