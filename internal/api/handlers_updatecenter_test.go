package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeUpdateCenterClient wraps fakeResourceClient to allow overriding
// GetUpdateCenter with a configurable return value.
type fakeUpdateCenterClient struct {
	fakeResourceClient
	uc  *v1alpha1.UpdateCenter
	err error
}

// storeFromUC builds the crdstore read surface mirroring the fake's uc/err.
func storeFromUC(f *fakeUpdateCenterClient) *crdstore.Fake {
	st := crdstore.NewFake()
	if f.uc != nil {
		u := f.uc.DeepCopy()
		if u.Name == "" {
			u.Name = "varroa-update-center"
		}
		crdstore.MustSeed(st, u)
	}
	if f.err != nil {
		if gvr, err := crdstore.GVRFor[v1alpha1.UpdateCenter](); err == nil {
			st.FailAlways("get", gvr, f.err)
		}
	}
	return st
}

func (f *fakeUpdateCenterClient) GetUpdateCenter(_ context.Context, _ string) (*v1alpha1.UpdateCenter, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.uc != nil {
		return f.uc, nil
	}
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("updatecenters"), "varroa-update-center")
}

// fakeInventoryClient is a call-counting fake implementing UpdateCenterInventory.
type fakeInventoryClient struct {
	entries []UpdateCenterInventoryEntry
	err     error
	calls   int
}

func (f *fakeInventoryClient) List(_ context.Context) ([]UpdateCenterInventoryEntry, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func testUpdateCenter(phase v1alpha1.UpdateCenterPhase, conditions []v1alpha1.UpdateCenterCondition, gaps []v1alpha1.UpdateCenterGap) *v1alpha1.UpdateCenter {
	return &v1alpha1.UpdateCenter{
		ObjectMeta: metav1.ObjectMeta{Name: "varroa-update-center"},
		Spec: v1alpha1.UpdateCenterSpec{
			Storage: v1alpha1.UpdateCenterStorage{
				Type: "oci",
			},
			PullThrough: v1alpha1.UpdateCenterPullThrough{
				Enabled: true,
			},
		},
		Status: v1alpha1.UpdateCenterStatus{
			Phase:        phase,
			Conditions:   conditions,
			PluginCount:  3,
			StoreBytes:   1048576,
			Gaps:         gaps,
			LastSyncTime: metav1.Time{Time: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
	}
}

var testConditions = []v1alpha1.UpdateCenterCondition{
	{Type: "Available", Status: metav1.ConditionTrue, LastTransitionTime: metav1.Time{Time: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}, Reason: "Available", Message: "update center is available"},
	{Type: "Reconciling", Status: metav1.ConditionFalse, LastTransitionTime: metav1.Time{Time: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}, Reason: "Reconciled", Message: "update center is reconciled"},
	{Type: "Stale", Status: metav1.ConditionFalse, LastTransitionTime: metav1.Time{Time: time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)}, Reason: "Fresh", Message: "inventory is fresh"},
	{Type: "Paused", Status: metav1.ConditionFalse, LastTransitionTime: metav1.Time{Time: time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC)}, Reason: "Active", Message: "update center is active"},
}

var testGaps = []v1alpha1.UpdateCenterGap{
	{Plugin: "blueocean", Version: "1.25.3", RequiredBy: "profile-a"},
	{Plugin: "workflow-api", Version: "2.47", RequiredBy: "profile-b"},
}

// ---------------------------------------------------------------------------
// Test: HandleUpdateCenterStatus
// ---------------------------------------------------------------------------

func TestHandleUpdateCenterStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		client     *fakeUpdateCenterClient
		wantStatus int
		checkResp  func(t *testing.T, resp UpdateCenterStatusResponse)
	}{
		{
			name: "CR absent - returns enabled:false with non-nil conditions/gaps and null lastSyncTime",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 nil,
				err:                nil, // triggers not-found path
			},
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterStatusResponse) {
				if resp.Enabled {
					t.Error("expected Enabled=false")
				}
				if resp.Conditions == nil {
					t.Error("expected Conditions to be non-nil []")
				}
				if resp.Gaps == nil {
					t.Error("expected Gaps to be non-nil []")
				}
				if resp.LastSyncTime != nil {
					t.Error("expected LastSyncTime to be nil")
				}
			},
		},
		{
			name: "phase Pending with all four condition types",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Pending"), testConditions, nil),
			},
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterStatusResponse) {
				if !resp.Enabled {
					t.Error("expected Enabled=true")
				}
				if resp.Phase != "Pending" {
					t.Errorf("expected Phase=Pending, got %q", resp.Phase)
				}
				if len(resp.Conditions) != 4 {
					t.Fatalf("expected 4 conditions, got %d", len(resp.Conditions))
				}
				// Check each condition type is mapped.
				types := make(map[string]bool)
				for _, c := range resp.Conditions {
					types[c.Type] = true
					if c.LastTransitionTime == nil {
						t.Errorf("condition %q has nil LastTransitionTime", c.Type)
					}
				}
				for _, want := range []string{"Available", "Reconciling", "Stale", "Paused"} {
					if !types[want] {
						t.Errorf("missing condition type %q", want)
					}
				}
			},
		},
		{
			name: "phase Ready",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Ready"), testConditions, nil),
			},
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterStatusResponse) {
				if resp.Phase != "Ready" {
					t.Errorf("expected Phase=Ready, got %q", resp.Phase)
				}
				if resp.PluginCount != 3 {
					t.Errorf("expected PluginCount=3, got %d", resp.PluginCount)
				}
				if resp.StoreBytes != 1048576 {
					t.Errorf("expected StoreBytes=1048576, got %d", resp.StoreBytes)
				}
				if resp.LastSyncTime == nil {
					t.Error("expected non-nil LastSyncTime")
				}
				if resp.StorageType != "oci" {
					t.Errorf("expected StorageType=oci, got %q", resp.StorageType)
				}
				if !resp.PullThroughEnabled {
					t.Error("expected PullThroughEnabled=true")
				}
			},
		},
		{
			name: "phase Degraded",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Degraded"), testConditions, testGaps),
			},
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterStatusResponse) {
				if resp.Phase != "Degraded" {
					t.Errorf("expected Phase=Degraded, got %q", resp.Phase)
				}
				if len(resp.Gaps) != 2 {
					t.Fatalf("expected 2 gaps, got %d", len(resp.Gaps))
				}
				if resp.Gaps[0].Plugin != "blueocean" {
					t.Errorf("expected gap[0].Plugin=blueocean, got %q", resp.Gaps[0].Plugin)
				}
			},
		},
		{
			name: "phase Error still enabled:true",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Error"), testConditions, nil),
			},
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterStatusResponse) {
				if !resp.Enabled {
					t.Error("expected Enabled=true even when phase=Error")
				}
				if resp.Phase != "Error" {
					t.Errorf("expected Phase=Error, got %q", resp.Phase)
				}
			},
		},
		{
			name: "non-not-found read error returns 500",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				err:                errors.New("transient API error"),
			},
			wantStatus: http.StatusInternalServerError,
			checkResp: func(t *testing.T, resp UpdateCenterStatusResponse) {
				// Just checking status code; body is a plain error map.
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := &Dependencies{
				Client: tc.client,
				Store:  storeFromUC(tc.client),
				Logger: slog.Default(),
			}
			srv := NewServer(deps)
			req := httptest.NewRequest(http.MethodGet, "/updatecenter", nil)
			w := httptest.NewRecorder()
			srv.HandleUpdateCenterStatus(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d; body: %s", tc.wantStatus, w.Code, w.Body.String())
			}

			if tc.wantStatus == http.StatusOK {
				var resp UpdateCenterStatusResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("unmarshal response: %v", err)
				}
				tc.checkResp(t, resp)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: HandleUpdateCenterPlugins
// ---------------------------------------------------------------------------

func TestHandleUpdateCenterPlugins(t *testing.T) {
	t.Parallel()

	inv := []UpdateCenterInventoryEntry{
		{Name: "blueocean", Version: "1.25.3", SHA256: "abc123", SizeBytes: 1024},
		{Name: "workflow-api", Version: "2.47", SHA256: "def456", SizeBytes: 2048},
		{Name: "credentials", Version: "2.0.1", SHA256: "ghi789", SizeBytes: 512},
	}

	tests := []struct {
		name         string
		client       *fakeUpdateCenterClient
		inventory    *fakeInventoryClient
		query        string
		wantStatus   int
		checkResp    func(t *testing.T, resp UpdateCenterPluginsResponse)
		wantInvCalls int // expected number of inventory.List calls
	}{
		{
			name: "CR absent - returns enabled:false, plugins:[] and inventory NOT called",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 nil,
				err:                nil, // triggers not-found path
			},
			inventory:  &fakeInventoryClient{entries: inv},
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterPluginsResponse) {
				if resp.Enabled {
					t.Error("expected Enabled=false")
				}
				if resp.Plugins == nil {
					t.Error("expected Plugins to be non-nil []")
				}
				if len(resp.Plugins) != 0 {
					t.Errorf("expected 0 plugins, got %d", len(resp.Plugins))
				}
			},
			wantInvCalls: 0,
		},
		{
			name: "CR present + nil inventory dependency returns 502",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Ready"), nil, nil),
				err:                nil,
			},
			inventory:    nil, // nil dependency
			wantStatus:   http.StatusBadGateway,
			wantInvCalls: 0,
			checkResp:    func(t *testing.T, resp UpdateCenterPluginsResponse) {},
		},
		{
			name: "CR present + inventory error returns 502",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Ready"), nil, nil),
				err:                nil,
			},
			inventory: &fakeInventoryClient{
				entries: nil,
				err:     errors.New("inventory unavailable"),
			},
			wantStatus:   http.StatusBadGateway,
			wantInvCalls: 1,
			checkResp:    func(t *testing.T, resp UpdateCenterPluginsResponse) {},
		},
		{
			name: "CR present + q= filter - case-insensitive substring on name",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Ready"), nil, nil),
				err:                nil,
			},
			inventory:  &fakeInventoryClient{entries: inv},
			query:      "Blue", // mixed-case, should match "blueocean"
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterPluginsResponse) {
				if !resp.Enabled {
					t.Error("expected Enabled=true")
				}
				if len(resp.Plugins) != 1 {
					t.Fatalf("expected 1 plugin, got %d: %+v", len(resp.Plugins), resp.Plugins)
				}
				if resp.Plugins[0].Name != "blueocean" {
					t.Errorf("expected plugin name blueocean, got %q", resp.Plugins[0].Name)
				}
			},
			wantInvCalls: 1,
		},
		{
			name: "CR present + no q - unfiltered listing",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				uc:                 testUpdateCenter(v1alpha1.UpdateCenterPhase("Ready"), nil, nil),
				err:                nil,
			},
			inventory:  &fakeInventoryClient{entries: inv},
			query:      "",
			wantStatus: http.StatusOK,
			checkResp: func(t *testing.T, resp UpdateCenterPluginsResponse) {
				if len(resp.Plugins) != 3 {
					t.Fatalf("expected 3 plugins, got %d", len(resp.Plugins))
				}
				names := make(map[string]bool)
				for _, p := range resp.Plugins {
					names[p.Name] = true
				}
				for _, want := range []string{"blueocean", "workflow-api", "credentials"} {
					if !names[want] {
						t.Errorf("missing plugin %q", want)
					}
				}
			},
			wantInvCalls: 1,
		},
		{
			name: "non-not-found CR read error returns 500",
			client: &fakeUpdateCenterClient{
				fakeResourceClient: *newFakeResourceClient(),
				err:                errors.New("transient API error"),
			},
			inventory:    &fakeInventoryClient{entries: inv},
			wantStatus:   http.StatusInternalServerError,
			wantInvCalls: 0,
			checkResp:    func(t *testing.T, resp UpdateCenterPluginsResponse) {},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			deps := &Dependencies{
				Client: tc.client,
				Store:  storeFromUC(tc.client),
				Logger: slog.Default(),
			}
			if tc.inventory != nil {
				deps.UpdateCenterInventory = tc.inventory
			}
			srv := NewServer(deps)
			path := "/updatecenter/plugins"
			if tc.query != "" {
				path = "/updatecenter/plugins?q=" + tc.query
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			srv.HandleUpdateCenterPlugins(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d; body: %s", tc.wantStatus, w.Code, w.Body.String())
			}

			// Check inventory call count.
			if tc.inventory != nil && tc.inventory.calls != tc.wantInvCalls {
				t.Errorf("expected inventory.List called %d times, got %d", tc.wantInvCalls, tc.inventory.calls)
			}

			if tc.wantStatus == http.StatusOK {
				var resp UpdateCenterPluginsResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("unmarshal response: %v", err)
				}
				tc.checkResp(t, resp)
			}
		})
	}
}

// contractUploadContentType and contractUploadBody build the minimal valid
// multipart/form-data upload the spec requires, so the contract harness
// exercises the real request shape rather than a JSON stand-in.
const contractUploadBody0 = "--varroa-contract\r\n" +
	"Content-Disposition: form-data; name=\"file\"; filename=\"plugin.hpi\"\r\n" +
	"Content-Type: application/octet-stream\r\n\r\n" +
	"not-a-real-hpi\r\n" +
	"--varroa-contract--\r\n"

const contractUploadContentType = "multipart/form-data; boundary=varroa-contract"

func contractUploadBody() []byte { return []byte(contractUploadBody0) }

// Contract test cases for update center routes.
func init() {
	// The default fakeResourceClient.GetUpdateCenter returns nil,nil which
	// the handler treats as IsNotFound, yielding the disabled 200 shape.
	registerContractCases(
		contractCase{Name: "getUpdateCenterStatus", Method: "GET", Path: "/api/v1/updatecenter", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getUpdateCenterPlugins", Method: "GET", Path: "/api/v1/updatecenter/plugins", Claims: adminClaims, WantStatus: http.StatusOK},
		// No UpdateCenter CR exists in the contract fixture, so the upload is
		// refused before the body is ever read: uploading to a disabled update
		// center is a client error, not the Enabled:false read shape the GET
		// handlers return.
		contractCase{
			Name: "uploadUpdateCenterPlugin", Method: "POST", Path: "/api/v1/updatecenter/plugins",
			RawBody: contractUploadBody(), ContentType: contractUploadContentType,
			Claims: adminClaims, WantStatus: http.StatusConflict,
		},
	)
}
