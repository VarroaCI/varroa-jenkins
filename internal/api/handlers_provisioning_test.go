package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// fakeProvisioningClient extends the standard fake with configurable provisioning defaults.
type fakeProvisioningClient struct {
	fakeResourceClient
	provisioning *v1alpha1.ProvisioningDefaults
	profiles     []*v1alpha1.JenkinsVersionProfile
	listErr      error
	// storeFrom builds the crdstore read surface mirroring this fake's fields.
	// configMaps keyed by ConfigMap name → data map; configMapErr forces an error.
	configMaps   map[string]map[string]string
	configMapErr error
}

func storeFromProvisioning(f *fakeProvisioningClient) *crdstore.Fake {
	// The profiles field replaces the base fake's version-profile set (the old
	// fake's ListJenkinsVersionProfileCRDs override had the same semantics).
	fc := f.fakeResourceClient
	fc.versionProfiles = nil
	st := storeFromFake(&fc)
	if f.provisioning != nil {
		d := f.provisioning.DeepCopy()
		if d.Name == "" {
			d.Name = "varroa-defaults"
		}
		crdstore.MustSeed(st, d)
	}
	for _, p := range f.profiles {
		crdstore.MustSeed(st, p)
	}
	if f.listErr != nil {
		if gvr, err := crdstore.GVRFor[v1alpha1.JenkinsVersionProfile](); err == nil {
			st.FailAlways("list", gvr, f.listErr)
		}
	}
	return st
}

func (f *fakeProvisioningClient) GetProvisioningDefaultsCRD(_ context.Context, _ string) (*v1alpha1.ProvisioningDefaults, error) {
	if f.provisioning != nil {
		return f.provisioning, nil
	}
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("provisioningdefaults"), "varroa-defaults")
}

func (f *fakeProvisioningClient) ListJenkinsVersionProfileCRDs(_ context.Context) ([]*v1alpha1.JenkinsVersionProfile, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.profiles, nil
}

func (f *fakeProvisioningClient) GetConfigMap(_ context.Context, name, _ string) (map[string]string, error) {
	if f.configMapErr != nil {
		return nil, f.configMapErr
	}
	if cm, ok := f.configMaps[name]; ok {
		return cm, nil
	}
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("configmaps"), name)
}

func TestHandleProvisioningConfig(t *testing.T) {
	t.Run("all fields present", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
			provisioning: &v1alpha1.ProvisioningDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
				Spec: v1alpha1.ProvisioningDefaultsSpec{
					RootDomain:     "jenkins.example.com",
					DefaultVersion: "2.479.3",
					SizePresets: []v1alpha1.SizePreset{
						{Name: "S", CPU: "1", Memory: "2Gi", Storage: "10Gi"},
					},
				},
			},
			profiles: []*v1alpha1.JenkinsVersionProfile{
				{ObjectMeta: metav1.ObjectMeta{Name: "v2-479-3"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.479.3", Channel: "lts", Recommended: true}},
				{ObjectMeta: metav1.ObjectMeta{Name: "v2-462-3"}, Spec: v1alpha1.JenkinsVersionProfileSpec{Version: "2.462.3", Channel: "lts", EOL: "2026-10-01"}},
			},
		}
		deps := &Dependencies{
			Client: client,
			Store:  storeFromProvisioning(client),
			Logger: slog.Default(),
		}
		srv := NewServer(deps)
		req := httptest.NewRequest(http.MethodGet, "/provisioning/config", nil)
		w := httptest.NewRecorder()
		srv.HandleProvisioningConfig(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp provisioningConfigResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.RootDomain != "jenkins.example.com" {
			t.Errorf("rootDomain: got %q, want %q", resp.RootDomain, "jenkins.example.com")
		}
		if resp.DefaultNamespace != "varroa" {
			t.Errorf("defaultNamespace: got %q, want %q", resp.DefaultNamespace, "varroa")
		}
		if resp.DefaultVersion != "2.479.3" {
			t.Errorf("defaultVersion: got %q, want %q", resp.DefaultVersion, "2.479.3")
		}
		if len(resp.Namespaces) != 1 || resp.Namespaces[0] != "varroa" {
			t.Errorf("namespaces: got %v, want [varroa]", resp.Namespaces)
		}
		if len(resp.Versions) != 2 {
			t.Errorf("versions length: got %d, want 2", len(resp.Versions))
		}
		if len(resp.SizePresets) != 1 {
			t.Errorf("sizePresets length: got %d, want 1", len(resp.SizePresets))
		}
		if len(resp.InjectedVariables) != len(bundle.InjectedVariableNames) {
			t.Errorf("injectedVariables length: got %d, want %d", len(resp.InjectedVariables), len(bundle.InjectedVariableNames))
		}
	})

	t.Run("unset spec returns varroa default and empty arrays", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
			provisioning: &v1alpha1.ProvisioningDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
			},
		}
		deps := &Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()}
		srv := NewServer(deps)
		req := httptest.NewRequest(http.MethodGet, "/provisioning/config", nil)
		w := httptest.NewRecorder()
		srv.HandleProvisioningConfig(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp provisioningConfigResponse
		json.Unmarshal(w.Body.Bytes(), &resp)

		if resp.DefaultNamespace != "varroa" {
			t.Errorf("defaultNamespace: got %q", resp.DefaultNamespace)
		}
		if resp.Namespaces == nil {
			t.Error("namespaces should not be null")
		}
		if resp.Versions == nil {
			t.Error("versions should not be null")
		}
		if resp.SizePresets == nil {
			t.Error("sizePresets should not be null")
		}
	})

	t.Run("not found defaults returns zero-value config", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
		}
		deps := &Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()}
		srv := NewServer(deps)
		req := httptest.NewRequest(http.MethodGet, "/provisioning/config", nil)
		w := httptest.NewRecorder()
		srv.HandleProvisioningConfig(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var resp provisioningConfigResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.DefaultNamespace != "varroa" {
			t.Errorf("expected varroa, got %q", resp.DefaultNamespace)
		}
	})

	t.Run("custom defaultNamespace", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
			provisioning: &v1alpha1.ProvisioningDefaults{
				ObjectMeta: metav1.ObjectMeta{Name: "varroa-defaults"},
				Spec: v1alpha1.ProvisioningDefaultsSpec{
					DefaultNamespace: "jenkins",
					Namespaces:       []string{"jenkins", "cicd"},
				},
			},
		}
		deps := &Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()}
		srv := NewServer(deps)
		req := httptest.NewRequest(http.MethodGet, "/provisioning/config", nil)
		w := httptest.NewRecorder()
		srv.HandleProvisioningConfig(w, req)

		var resp provisioningConfigResponse
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.DefaultNamespace != "jenkins" {
			t.Errorf("defaultNamespace: got %q", resp.DefaultNamespace)
		}
		if len(resp.Namespaces) != 2 || resp.Namespaces[0] != "jenkins" || resp.Namespaces[1] != "cicd" {
			t.Errorf("namespaces: got %v, want [jenkins cicd]", resp.Namespaces)
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
		}
		deps := &Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()}
		srv := NewServer(deps)
		req := httptest.NewRequest(http.MethodPost, "/provisioning/config", nil)
		w := httptest.NewRecorder()
		srv.HandleProvisioningConfig(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})
}

// --- Task 2.1: catalog entry additions + ordering ---

func TestBuildVersionCatalog_EntryAdditions(t *testing.T) {
	profiles := []*v1alpha1.JenkinsVersionProfile{
		// ready: pluginSetRef + PluginSetReady=True
		{
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins-version-2-552"},
			Spec: v1alpha1.JenkinsVersionProfileSpec{
				Version: "2.552", Channel: "lts", Recommended: true,
				PluginSetRef: &v1alpha1.ConfigMapRef{Name: "jenkins-version-2-552-pluginset"},
			},
			Status: v1alpha1.JenkinsVersionProfileStatus{
				PluginCount: 71,
				Conditions: []v1alpha1.JenkinsVersionProfileCondition{
					{Type: "PluginSetReady", Status: metav1.ConditionTrue},
				},
			},
		},
		// unready: pluginSetRef but no/False condition
		{
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins-version-2-540"},
			Spec: v1alpha1.JenkinsVersionProfileSpec{
				Version: "2.540", Channel: "lts",
				PluginSetRef: &v1alpha1.ConfigMapRef{Name: "jenkins-version-2-540-pluginset"},
			},
			Status: v1alpha1.JenkinsVersionProfileStatus{PluginCount: 0},
		},
		// metadata-only: no pluginSetRef
		{
			ObjectMeta: metav1.ObjectMeta{Name: "jenkins-version-2-570"},
			Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.570", Channel: "weekly"},
		},
	}
	client := &fakeProvisioningClient{fakeResourceClient: *newFakeResourceClient(), profiles: profiles}
	srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()})
	entries := srv.buildVersionCatalog(context.Background())

	byName := map[string]VersionCatalogEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	// ready
	ready := byName["jenkins-version-2-552"]
	if ready.PluginSetReady == nil || *ready.PluginSetReady != true {
		t.Errorf("ready: pluginSetReady = %v, want true", ready.PluginSetReady)
	}
	if ready.PluginCount != 71 {
		t.Errorf("ready: pluginCount = %d, want 71", ready.PluginCount)
	}
	if ready.Name != "jenkins-version-2-552" {
		t.Errorf("ready: name = %q", ready.Name)
	}
	// unready
	unready := byName["jenkins-version-2-540"]
	if unready.PluginSetReady == nil || *unready.PluginSetReady != false {
		t.Errorf("unready: pluginSetReady = %v, want false", unready.PluginSetReady)
	}
	// metadata-only
	meta := byName["jenkins-version-2-570"]
	if meta.PluginSetReady != nil {
		t.Errorf("metadata-only: pluginSetReady = %v, want nil", meta.PluginSetReady)
	}

	// Assert JSON presence/absence of pluginSetReady.
	readyJSON, _ := json.Marshal(ready)
	if !contains(string(readyJSON), `"pluginSetReady":true`) {
		t.Errorf("ready JSON missing pluginSetReady: %s", readyJSON)
	}
	unreadyJSON, _ := json.Marshal(unready)
	if !contains(string(unreadyJSON), `"pluginSetReady":false`) {
		t.Errorf("unready JSON missing pluginSetReady:false: %s", unreadyJSON)
	}
	metaJSON, _ := json.Marshal(meta)
	if contains(string(metaJSON), "pluginSetReady") {
		t.Errorf("metadata-only JSON should omit pluginSetReady: %s", metaJSON)
	}
}

func TestBuildVersionCatalog_OrderingDesc(t *testing.T) {
	mk := func(v string) *v1alpha1.JenkinsVersionProfile {
		return &v1alpha1.JenkinsVersionProfile{
			ObjectMeta: metav1.ObjectMeta{Name: "p-" + v},
			Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: v, Channel: "lts"},
		}
	}
	// Shuffled input.
	profiles := []*v1alpha1.JenkinsVersionProfile{mk("2.540"), mk("2.570"), mk("2.552"), mk("2.552.3")}
	client := &fakeProvisioningClient{fakeResourceClient: *newFakeResourceClient(), profiles: profiles}
	srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()})
	entries := srv.buildVersionCatalog(context.Background())

	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Version)
	}
	want := []string{"2.570", "2.552.3", "2.552", "2.540"}
	if len(got) != len(want) {
		t.Fatalf("length: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("order[%d]: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// --- Tasks 2.2 + 2.3: GET /version-profiles ---

func TestHandleVersionProfiles(t *testing.T) {
	fullProfile := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-version-2-552"},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version: "2.552", Channel: "lts", Recommended: true,
			PluginSetRef: &v1alpha1.ConfigMapRef{Name: "jenkins-version-2-552-pluginset"},
			JCasC:        &v1alpha1.VersionJCasC{RequiredPlugins: []string{"role-strategy"}},
		},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "jenkins-version-2-552-pluginset-content", PluginCount: 2,
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{
				{Type: "PluginSetReady", Status: metav1.ConditionTrue, Reason: "Materialized", Message: "ok"},
				{Type: "LockJcascMismatch", Status: metav1.ConditionFalse, Reason: "InSync", Message: "aligned"},
			},
		},
	}
	metaOnly := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-version-2-570"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.570", Channel: "weekly"},
	}
	contentCM := map[string]map[string]string{
		"jenkins-version-2-552-pluginset-content": {
			"plugins.yaml": "core:\n  - configuration-as-code\nplugins:\n  - artifactId: role-strategy\n    version: 742.vb\n  - artifactId: instance-identity\n    version: \"\"\n",
		},
	}

	t.Run("full detail with plugins", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
			profiles:           []*v1alpha1.JenkinsVersionProfile{fullProfile},
			configMaps:         contentCM,
		}
		srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default(), OperatorNamespace: "varroa"})
		req := httptest.NewRequest(http.MethodGet, "/version-profiles", nil)
		w := httptest.NewRecorder()
		srv.HandleVersionProfiles(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var envelope struct {
			Items []VersionProfileDetail `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(envelope.Items) != 1 {
			t.Fatalf("expected 1 profile, got %d", len(envelope.Items))
		}
		d := envelope.Items[0]
		if !d.HasJcasc {
			t.Error("hasJcasc should be true")
		}
		if len(d.RequiredPlugins) != 1 || d.RequiredPlugins[0] != "role-strategy" {
			t.Errorf("requiredPlugins: got %v", d.RequiredPlugins)
		}
		if len(d.Conditions) != 2 {
			t.Fatalf("expected 2 conditions, got %d", len(d.Conditions))
		}
		if d.Conditions[0].Type != "PluginSetReady" || d.Conditions[0].Status != "True" || d.Conditions[0].Reason != "Materialized" {
			t.Errorf("condition[0] = %+v", d.Conditions[0])
		}
		wantPlugins := []string{"role-strategy@742.vb", "instance-identity"}
		if len(d.Plugins) != len(wantPlugins) {
			t.Fatalf("plugins: got %v, want %v", d.Plugins, wantPlugins)
		}
		for i := range wantPlugins {
			if d.Plugins[i] != wantPlugins[i] {
				t.Errorf("plugins[%d]: got %q, want %q", i, d.Plugins[i], wantPlugins[i])
			}
		}
		if len(d.Plugins) != d.PluginCount {
			t.Errorf("len(plugins)=%d != pluginCount=%d", len(d.Plugins), d.PluginCount)
		}
	})

	t.Run("metadata-only omits fields", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
			profiles:           []*v1alpha1.JenkinsVersionProfile{metaOnly},
		}
		srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default(), OperatorNamespace: "varroa"})
		req := httptest.NewRequest(http.MethodGet, "/version-profiles", nil)
		w := httptest.NewRecorder()
		srv.HandleVersionProfiles(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !contains(body, `"hasJcasc":false`) {
			t.Errorf("expected hasJcasc:false in %s", body)
		}
		if !contains(body, `"conditions":[]`) {
			t.Errorf("expected conditions:[] in %s", body)
		}
		if contains(body, "pluginSetRef") || contains(body, "contentRef") || contains(body, "plugins") || contains(body, "requiredPlugins") {
			t.Errorf("metadata-only should omit pluginSetRef/contentRef/plugins/requiredPlugins: %s", body)
		}
	})

	// Issue #416: a profile whose contentRef is set but unreadable used to be
	// reported as 200-with-plugins-absent, indistinguishable from a genuinely
	// unmaterialized profile — which is how `varroactl export plugins`
	// published an empty pack and exited 0. contentRef being set ASSERTS the
	// plugin set exists, so failing to read it is a broken cluster state.
	t.Run("unreadable plugin set is 500 naming the profile", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
			profiles:           []*v1alpha1.JenkinsVersionProfile{fullProfile},
			configMapErr:       errors.New("configmaps is forbidden"),
		}
		srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default(), OperatorNamespace: "varroa"})
		req := httptest.NewRequest(http.MethodGet, "/version-profiles", nil)
		w := httptest.NewRecorder()
		srv.HandleVersionProfiles(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !contains(body, "jenkins-version-2-552") {
			t.Errorf("error must name the profile: %s", body)
		}
		if !contains(body, "configmaps is forbidden") {
			t.Errorf("error must carry the underlying cause: %s", body)
		}
	})

	t.Run("materialized but empty plugin set is 500", func(t *testing.T) {
		for name, data := range map[string]map[string]string{
			"missing plugins.yaml": {"other.yaml": "x"},
			"empty plugins.yaml":   {"plugins.yaml": ""},
			"no plugin lines":      {"plugins.yaml": "core: []\nplugins: []\n"},
		} {
			t.Run(name, func(t *testing.T) {
				client := &fakeProvisioningClient{
					fakeResourceClient: *newFakeResourceClient(),
					profiles:           []*v1alpha1.JenkinsVersionProfile{fullProfile},
					configMaps:         map[string]map[string]string{"jenkins-version-2-552-pluginset-content": data},
				}
				srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default(), OperatorNamespace: "varroa"})
				req := httptest.NewRequest(http.MethodGet, "/version-profiles", nil)
				w := httptest.NewRecorder()
				srv.HandleVersionProfiles(w, req)
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
				}
				if !contains(w.Body.String(), "jenkins-version-2-552") {
					t.Errorf("error must name the profile: %s", w.Body.String())
				}
			})
		}
	})

	// A profile with NO contentRef is a legitimate not-yet-materialized state
	// and must stay a 200. That distinction is the whole point of the fix.
	t.Run("no contentRef is 200 with no plugins", func(t *testing.T) {
		client := &fakeProvisioningClient{
			fakeResourceClient: *newFakeResourceClient(),
			profiles:           []*v1alpha1.JenkinsVersionProfile{metaOnly},
			configMapErr:       errors.New("configmaps is forbidden"),
		}
		srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default(), OperatorNamespace: "varroa"})
		req := httptest.NewRequest(http.MethodGet, "/version-profiles", nil)
		w := httptest.NewRecorder()
		srv.HandleVersionProfiles(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if contains(w.Body.String(), `"plugins"`) {
			t.Errorf("an unmaterialized profile carries no plugins: %s", w.Body.String())
		}
	})

	t.Run("empty returns []", func(t *testing.T) {
		client := &fakeProvisioningClient{fakeResourceClient: *newFakeResourceClient()}
		srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()})
		req := httptest.NewRequest(http.MethodGet, "/version-profiles", nil)
		w := httptest.NewRecorder()
		srv.HandleVersionProfiles(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var envelope struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(envelope.Items) != 0 {
			t.Errorf("expected empty items, got %d", len(envelope.Items))
		}
	})

	t.Run("POST is 405", func(t *testing.T) {
		client := &fakeProvisioningClient{fakeResourceClient: *newFakeResourceClient()}
		srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()})
		req := httptest.NewRequest(http.MethodPost, "/version-profiles", nil)
		w := httptest.NewRecorder()
		srv.HandleVersionProfiles(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("expected 405, got %d", w.Code)
		}
	})
}

func TestHandleVersionProfiles_ListErrorIs500(t *testing.T) {
	client := &fakeProvisioningClient{fakeResourceClient: *newFakeResourceClient(), listErr: errors.New("list failed")}
	srv := NewServer(&Dependencies{Client: client, Store: storeFromProvisioning(client), Logger: slog.Default()})
	req := httptest.NewRequest(http.MethodGet, "/version-profiles", nil)
	w := httptest.NewRecorder()
	srv.HandleVersionProfiles(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// TestDispatchVersionProfiles_ClusterPrefixed_ExpandsPluginSetRef exercises the
// full dispatch path through NewRouter: GET /api/v1/clusters/core/version-profiles.
// This tests that pluginSetRef profiles have their plugins expanded via ContentRef,
// both through the local path and the bus/ConfigBrood path.
func TestDispatchVersionProfiles_ClusterPrefixed_ExpandsPluginSetRef(t *testing.T) {
	profile := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-version-2-570"},
		Spec: v1alpha1.JenkinsVersionProfileSpec{
			Version:        "2.570",
			Channel:        "lts",
			ResolveVersion: "2.570.1",
			PluginSetRef:   &v1alpha1.ConfigMapRef{Name: "jenkins-version-2-570-pluginset"},
		},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef:  "jenkins-version-2-570-pluginset-content",
			PluginCount: 72,
		},
	}
	contentCM := map[string]map[string]string{
		"jenkins-version-2-570-pluginset-content": {
			"plugins.yaml": `core:
  - configuration-as-code
plugins:
  - artifactId: role-strategy
    version: 867.vf2b_al_266a_d0c
  - artifactId: workflow-aggregator
    version: 608.v6c5a_4c5a_0085
`,
		},
	}
	client := &fakeProvisioningClient{
		fakeResourceClient: *newFakeResourceClient(),
		profiles:           []*v1alpha1.JenkinsVersionProfile{profile},
		configMaps:         contentCM,
	}
	deps := &Dependencies{
		Client:            client,
		Store:             storeFromProvisioning(client),
		Logger:            slog.Default(),
		OperatorNamespace: "varroa",
		Brood:             newFakeBrood(&client.fakeResourceClient),
		ConfigBrood:       &stubConfigBrood{client: &client.fakeResourceClient, operatorNs: "varroa"},
	}
	handler := NewRouter(deps) // strips /api/v1
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/clusters/core/version-profiles", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var envelope struct {
		Items []VersionProfileDetail `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.Items) != 1 {
		t.Fatalf("expected 1 profile, got %d", len(envelope.Items))
	}
	d := envelope.Items[0]
	if d.Name != "jenkins-version-2-570" {
		t.Errorf("name = %q, want %q", d.Name, "jenkins-version-2-570")
	}
	if d.ResolveVersion != "2.570.1" {
		t.Errorf("resolveVersion = %q, want %q", d.ResolveVersion, "2.570.1")
	}
	if d.PluginSetRef != "jenkins-version-2-570-pluginset" {
		t.Errorf("pluginSetRef = %q", d.PluginSetRef)
	}
	if d.ContentRef != "jenkins-version-2-570-pluginset-content" {
		t.Errorf("contentRef = %q", d.ContentRef)
	}
	if d.PluginCount != 72 {
		t.Errorf("pluginCount = %d, want 72 (from CR status)", d.PluginCount)
	}
	wantPlugins := []string{"role-strategy@867.vf2b_al_266a_d0c", "workflow-aggregator@608.v6c5a_4c5a_0085"}
	if len(d.Plugins) != len(wantPlugins) {
		t.Fatalf("plugins: got %v, want %v", d.Plugins, wantPlugins)
	}
	for i := range wantPlugins {
		if d.Plugins[i] != wantPlugins[i] {
			t.Errorf("plugins[%d]: got %q, want %q", i, d.Plugins[i], wantPlugins[i])
		}
	}
}

// TestVersionProfileDetail_JSONShapeUnchanged pins the wire shape of
// VersionProfileDetail. The #416 diagnostic is carried by the HTTP status, NOT
// by the body: VersionProfileDetail is part of the OpenAPI contract
// (api/openapi/components/schemas.yaml) and the generated client, so adding a
// field here — or dropping the omitempty that hides the empty case — would drag
// this change onto a regeneration surface it does not own.
func TestVersionProfileDetail_JSONShapeUnchanged(t *testing.T) {
	wantKeys := map[string]bool{
		"name": true, "version": true, "channel": true, "recommended": true,
		"eol": true, "pluginSetRef": true, "contentRef": true, "pluginCount": true,
		"resolveVersion": true, "plugins": true, "hasJcasc": true,
		"requiredPlugins": true, "conditions": true,
	}

	// Fully populated: exactly the known keys, nothing new.
	full := VersionProfileDetail{
		Name: "p", Version: "2.555", Channel: "lts", Recommended: true, EOL: "2027-01-01",
		PluginSetRef: "src", ContentRef: "content", PluginCount: 2, ResolveVersion: "2.555.1",
		Plugins: []string{"a@1"}, HasJcasc: true, RequiredPlugins: []string{"b"},
		Conditions: []VersionProfileCondition{},
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for k := range got {
		if !wantKeys[k] {
			t.Errorf("unexpected key %q — VersionProfileDetail must not grow a field in this change", k)
		}
	}
	for k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}

	// Nil Plugins must still be OMITTED, not serialized as [].
	bare := VersionProfileDetail{Name: "p", Version: "2.555", Channel: "lts", Conditions: []VersionProfileCondition{}}
	b2, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got2 map[string]any
	if err := json.Unmarshal(b2, &got2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got2["plugins"]; present {
		t.Errorf("plugins must keep its omitempty: %s", string(b2))
	}
}
