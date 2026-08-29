package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

func boolPtr(b bool) *bool { return &b }

// TestBuildVersionStatus covers the tri-state projection of the A/B version
// conditions into the detail DTO: neither present → nil; only one present; both.
func TestBuildVersionStatus(t *testing.T) {
	tests := []struct {
		name  string
		conds []v1alpha1.ControllerCondition
		want  *VersionStatusJSON
	}{
		{
			name:  "neither condition present",
			conds: nil,
			want:  nil,
		},
		{
			name: "unrelated conditions ignored",
			conds: []v1alpha1.ControllerCondition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllGood"},
			},
			want: nil,
		},
		{
			name: "only VersionRollPending (rolling)",
			conds: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionVersionRollPending, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVersionRollStarted, Message: "rolling to 2.555"},
			},
			want: &VersionStatusJSON{RollPending: boolPtr(true), RollReason: "VersionRollStarted", RollMessage: "rolling to 2.555"},
		},
		{
			name: "only VersionUpgradeBlocked",
			conds: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionVersionUpgradeBlocked, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonCoreOlderThanPluginBaseline, Message: "core older"},
			},
			want: &VersionStatusJSON{UpgradeBlocked: boolPtr(true), BlockedReason: "CoreOlderThanPluginBaseline", BlockedMessage: "core older"},
		},
		{
			name: "both present, roll false converged + blocked true",
			conds: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionVersionRollPending, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonVersionConverged},
				{Type: v1alpha1.ConditionVersionUpgradeBlocked, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonCoreOlderThanPluginBaseline, Message: "blocked"},
			},
			want: &VersionStatusJSON{
				RollPending:    boolPtr(false),
				RollReason:     "VersionConverged",
				UpgradeBlocked: boolPtr(true),
				BlockedReason:  "CoreOlderThanPluginBaseline",
				BlockedMessage: "blocked",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildVersionStatus(tt.conds)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("buildVersionStatus() = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestBuildPluginConflict covers the three BFF scenarios for PluginConflict
// projection: nil when never recorded, non-nil Active=true when True, non-nil
// Active=false (not omitted) when False.
func TestBuildPluginConflict(t *testing.T) {
	tests := []struct {
		name  string
		conds []v1alpha1.ControllerCondition
		want  *PluginConflictJSON
	}{
		{
			name:  "never recorded — nil",
			conds: nil,
			want:  nil,
		},
		{
			name: "unrelated conditions — nil",
			conds: []v1alpha1.ControllerCondition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllGood"},
			},
			want: nil,
		},
		{
			name: "condition True",
			conds: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionPluginConflict, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonPluginConflict, Message: "plugin foo: pin=1.0, lock=1.1"},
			},
			want: &PluginConflictJSON{Active: boolPtr(true), Reason: "PluginConflict", Message: "plugin foo: pin=1.0, lock=1.1"},
		},
		{
			name: "condition False — Active=false, not nil",
			conds: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionPluginConflict, Status: metav1.ConditionFalse, Reason: "NoConflict"},
			},
			want: &PluginConflictJSON{Active: boolPtr(false), Reason: "NoConflict"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPluginConflict(tt.conds)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("buildPluginConflict() = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestBuildMiteImageStatus covers the projection of MiteStatus.Image and the
// MiteImageStale condition into the detail DTO.
func TestBuildMiteImageStatus(t *testing.T) {
	image := "mite:running"
	tests := []struct {
		name  string
		ms    *v1alpha1.MiteStatus
		conds []v1alpha1.ControllerCondition
		want  *MiteImageStatusJSON
	}{
		{
			name: "nil MiteStatus",
			ms:   nil,
			want: nil,
		},
		{
			name: "empty Image",
			ms:   &v1alpha1.MiteStatus{Image: ""},
			want: nil,
		},
		{
			name: "image present, stale=true",
			ms:   &v1alpha1.MiteStatus{Image: "mite:running"},
			conds: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionMiteImageStale, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonMiteImageStale},
			},
			want: &MiteImageStatusJSON{Image: &image, Stale: boolPtr(true)},
		},
		{
			name: "image present, stale=false",
			ms:   &v1alpha1.MiteStatus{Image: "mite:running"},
			conds: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionMiteImageStale, Status: metav1.ConditionFalse, Reason: v1alpha1.ReasonMiteImageCurrent},
			},
			want: &MiteImageStatusJSON{Image: &image, Stale: boolPtr(false)},
		},
		{
			name: "image present, no MiteImageStale condition",
			ms:   &v1alpha1.MiteStatus{Image: "mite:running"},
			conds: []v1alpha1.ControllerCondition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllGood"},
			},
			want: &MiteImageStatusJSON{Image: &image},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildMiteImageStatus(tt.ms, tt.conds)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("buildMiteImageStatus() = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestListController_Version asserts spec.version surfaces as the list DTO's
// "version" field (desired version for the upgrade-drift column).
func TestListController_Version(t *testing.T) {
	srv, client := newRoutingTestServer()
	client.controllers["ci"] = &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.555"},
	}
	crdstore.MustSeed(client.crdStore, client.controllers["ci"])

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(envelope.Items) != 1 || envelope.Items[0]["version"] != "2.555" {
		t.Fatalf("expected version=2.555 in list, got %+v", envelope.Items)
	}
}

// TestDetailController_VersionStatus asserts the detail DTO omits versionStatus
// when no version condition is present and projects it when present.
func TestDetailController_VersionStatus(t *testing.T) {
	t.Run("omitted when no version conditions", func(t *testing.T) {
		srv, client := newRoutingTestServer()
		client.controllers["ci"] = &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{Version: "2.555"},
		}
		crdstore.MustSeed(client.crdStore, client.controllers["ci"])
		detail := getDetail(t, srv)
		if _, ok := detail["versionStatus"]; ok {
			t.Fatalf("expected versionStatus omitted, got %+v", detail["versionStatus"])
		}
	})

	t.Run("projected when VersionRollPending present", func(t *testing.T) {
		srv, client := newRoutingTestServer()
		client.controllers["ci"] = &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{Version: "2.555"},
			Status: v1alpha1.ControllerStatus{Conditions: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionVersionRollPending, Status: metav1.ConditionTrue, Reason: v1alpha1.ReasonVersionRollStarted, Message: "rolling"},
			}},
		}
		crdstore.MustSeed(client.crdStore, client.controllers["ci"])
		detail := getDetail(t, srv)
		vs, ok := detail["versionStatus"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected versionStatus object, got %+v", detail["versionStatus"])
		}
		if vs["rollPending"] != true || vs["rollReason"] != "VersionRollStarted" {
			t.Fatalf("unexpected versionStatus: %+v", vs)
		}
		if _, present := vs["upgradeBlocked"]; present {
			t.Fatalf("upgradeBlocked should be absent, got %+v", vs)
		}
	})
}

func getDetail(t *testing.T, srv *Server) map[string]interface{} {
	t.Helper()
	const ns, name = "team-a", "ci"
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers/"+ns+"/"+name, nil)
	srv.handleControllerDetail(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", ns, name)
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	return detail
}

// normalMiteTransport is a stub Transport that reports a connected mite whose
// snapshot carries the "NORMAL" Jenkins mode sentinel in the version slot.
type normalMiteTransport struct{ stubTransport }

func (n *normalMiteTransport) Snapshot(_, _ string) *mitev1.StateSnapshot {
	return &mitev1.StateSnapshot{JenkinsVersion: "NORMAL", JenkinsHealth: "healthy"}
}
func (n *normalMiteTransport) Info(_, _ string) (string, time.Time, time.Time, bool) {
	return "mite-1.0", time.Now(), time.Now(), true
}

// TestNormalVersionGuard asserts the "NORMAL" Jenkins mode sentinel is never
// surfaced as jenkinsVersion in either the list or detail responses.
func TestNormalVersionGuard(t *testing.T) {
	client := newFakeResourceClient()
	client.controllers = map[string]*v1alpha1.Controller{
		"ci": {
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{Version: "2.555"},
		},
	}
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromFake(client),
		Authorizer:        adminTestAuthorizer(),
		MiteRegistry:      &normalMiteTransport{},
		OperatorNamespace: "test-ns",
		Logger:            slog.Default(),
	})

	// List: jenkinsVersion must be unset.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/controllers", nil)
	srv.HandleControllers(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)))
	var envelope struct {
		Items []map[string]interface{} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(envelope.Items) != 1 {
		t.Fatalf("expected 1 controller, got %d", len(envelope.Items))
	}
	if _, ok := envelope.Items[0]["jenkinsVersion"]; ok {
		t.Errorf("list: jenkinsVersion should be dropped for NORMAL, got %+v", envelope.Items[0]["jenkinsVersion"])
	}

	// Detail: jenkinsVersion must be unset.
	detail := getDetail(t, srv)
	if _, ok := detail["jenkinsVersion"]; ok {
		t.Errorf("detail: jenkinsVersion should be dropped for NORMAL, got %+v", detail["jenkinsVersion"])
	}
}
