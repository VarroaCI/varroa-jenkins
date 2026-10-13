package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/bundle/pipelinetemplate"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// bundleTestClient fakes the k8s CRD client for bundle-specific
// unit tests (does not embed the full fakeResourceClient).
type bundleTestClient struct {
	*fakeResourceClient
	bundles map[string]*v1alpha1.ComposedBundle // key: ns + "/" + name
	items   map[string]*v1alpha1.CatalogItem    // key: ns + "/" + name
}

// storeFromBundleClient seeds a fake store with the embedded fake's CRDs
// plus this client's bundles and items.
func storeFromBundleClient(c *bundleTestClient) *crdstore.Fake {
	st := storeFromFake(c.fakeResourceClient)
	for _, b := range c.bundles {
		crdstore.MustSeed(st, b)
	}
	for _, it := range c.items {
		crdstore.MustSeed(st, it)
	}
	return st
}

func newBundleTestClient() *bundleTestClient {
	f := newFakeResourceClient()
	f.controllers = map[string]*v1alpha1.Controller{}
	return &bundleTestClient{
		fakeResourceClient: f,
		bundles:            map[string]*v1alpha1.ComposedBundle{},
		items:              map[string]*v1alpha1.CatalogItem{},
	}
}

func (c *bundleTestClient) addBundle(cb *v1alpha1.ComposedBundle) {
	c.bundles[cb.Namespace+"/"+cb.Name] = cb
}

func (c *bundleTestClient) addItem(it *v1alpha1.CatalogItem) {
	c.items[it.Namespace+"/"+it.Name] = it
}

func (c *bundleTestClient) GetComposedBundleCRD(_ context.Context, name, ns string) (*v1alpha1.ComposedBundle, error) {
	if cb, ok := c.bundles[ns+"/"+name]; ok {
		return cb, nil
	}
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("composedbundles"), name)
}

func (c *bundleTestClient) GetCatalogItemCRD(_ context.Context, name, ns string) (*v1alpha1.CatalogItem, error) {
	if it, ok := c.items[ns+"/"+name]; ok {
		return it, nil
	}
	return nil, k8serrors.NewNotFound(v1alpha1.Resource("catalogitems"), name)
}

func (c *bundleTestClient) ListComposedBundleCRDs(_ context.Context, ns string) ([]*v1alpha1.ComposedBundle, error) {
	var result []*v1alpha1.ComposedBundle
	for _, cb := range c.bundles {
		if ns == "" || cb.Namespace == ns {
			result = append(result, cb)
		}
	}
	return result, nil
}

func (c *bundleTestClient) CreateComposedBundleCRD(_ context.Context, cr *v1alpha1.ComposedBundle) error {
	k := cr.Namespace + "/" + cr.Name
	if _, exists := c.bundles[k]; exists {
		return k8serrors.NewAlreadyExists(v1alpha1.Resource("composedbundles"), cr.Name)
	}
	c.bundles[k] = cr
	return nil
}

func (c *bundleTestClient) UpdateComposedBundleCRD(_ context.Context, cr *v1alpha1.ComposedBundle) error {
	k := cr.Namespace + "/" + cr.Name
	if _, exists := c.bundles[k]; !exists {
		return k8serrors.NewNotFound(v1alpha1.Resource("composedbundles"), cr.Name)
	}
	c.bundles[k] = cr
	return nil
}

func (c *bundleTestClient) DeleteComposedBundleCRD(_ context.Context, name, ns string) error {
	delete(c.bundles, ns+"/"+name)
	return nil
}

func (c *bundleTestClient) ListCatalogItemCRDs(_ context.Context, ns, _ string) ([]*v1alpha1.CatalogItem, error) {
	var result []*v1alpha1.CatalogItem
	for _, it := range c.items {
		if ns == "" || it.Namespace == ns {
			result = append(result, it)
		}
	}
	return result, nil
}

func jcascItem(name, ns, content string) *v1alpha1.CatalogItem {
	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemJCasC},
		Status:     v1alpha1.CatalogItemStatus{Content: content, Valid: true},
	}
}

func pluginItem(name, content string) *v1alpha1.CatalogItem {
	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPlugin},
		Status:     v1alpha1.CatalogItemStatus{Content: content, Valid: true},
	}
}

// pipelineTemplateItem builds a pipeline-template CatalogItem in
// "tenant-a" whose content is one PipelineTemplate document, name and
// version taken from the document itself (design D2).
func pipelineTemplateItem(name, content string) *v1alpha1.CatalogItem {
	return &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-a"},
		Spec:       v1alpha1.CatalogItemSpec{Type: v1alpha1.CatalogItemPipelineTemplate},
		Status:     v1alpha1.CatalogItemStatus{Content: content, Valid: true},
	}
}

// testComposedBundlePreview mirrors the old ComposedBundlePreview for test use.
type testComposedBundlePreview struct {
	BundleYAML            string                        `json:"bundleYaml"`
	JenkinsYAML           string                        `json:"jenkinsYaml"`
	PluginsYAML           string                        `json:"pluginsYaml"`
	ItemsYAML             string                        `json:"itemsYaml"`
	RbacYAML              string                        `json:"rbacYaml"`
	PipelineTemplatesYAML string                        `json:"pipelineTemplatesYaml"`
	Missing               []string                      `json:"missing"`
	Drifted               []string                      `json:"drifted"`
	Warnings              []string                      `json:"warnings"`
	UnresolvedVariables   []string                      `json:"unresolvedVariables"`
	PinPreflight          bus.PinPreflightReport        `json:"pinPreflight"`
	PipelineTemplates     []bus.PipelineTemplateSummary `json:"pipelineTemplates"`
}

// newBundleParityServer builds a Server whose ConfigBrood is backed by
// the test client.
// nolint:unparam
func newBundleParityServer(client *bundleTestClient, operatorNS string) *Server {
	fakeBrood := &fakeConfigBroodForTest{client: client, operatorNamespace: operatorNS}
	return NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromBundleClient(client),
		Authorizer:        adminTestAuthorizer(),
		ConfigBrood:       fakeBrood,
		OperatorNamespace: operatorNS,
		Logger:            slog.Default(),
	})
}

// fakeConfigBroodForTest wraps a bundleTestClient into a ConfigBrood that
// uses a real composer for ComposeBundle.
type fakeConfigBroodForTest struct {
	client            *bundleTestClient
	operatorNamespace string
}

func (f *fakeConfigBroodForTest) ListComposedBundles(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	items, err := f.client.ListComposedBundleCRDs(ctx, ns)
	if err != nil {
		return nil, err
	}
	raw := make([]json.RawMessage, len(items))
	for i, cr := range items {
		r, _ := json.Marshal(cr)
		raw[i] = r
	}
	return raw, nil
}
func (f *fakeConfigBroodForTest) GetComposedBundle(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	cr, err := f.client.GetComposedBundleCRD(ctx, name, ns)
	if err != nil {
		return nil, err
	}
	return json.Marshal(cr)
}
func (f *fakeConfigBroodForTest) CreateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	var cb v1alpha1.ComposedBundle
	if err := json.Unmarshal(obj, &cb); err != nil {
		return nil, err
	}
	if err := f.client.CreateComposedBundleCRD(ctx, &cb); err != nil {
		return nil, err
	}
	return obj, nil
}
func (f *fakeConfigBroodForTest) UpdateComposedBundle(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	var cb v1alpha1.ComposedBundle
	if err := json.Unmarshal(obj, &cb); err != nil {
		return nil, err
	}
	if err := f.client.UpdateComposedBundleCRD(ctx, &cb); err != nil {
		return nil, err
	}
	return obj, nil
}
func (f *fakeConfigBroodForTest) DeleteComposedBundle(ctx context.Context, cluster, ns, name string) error {
	return f.client.DeleteComposedBundleCRD(ctx, name, ns)
}
func (f *fakeConfigBroodForTest) PauseComposedBundle(ctx context.Context, cluster, ns, name string, paused bool) error {
	return nil
}
func (f *fakeConfigBroodForTest) ComposeBundle(ctx context.Context, cluster, ns string, spec json.RawMessage) (*bus.BundleComposePreview, error) {
	var s v1alpha1.ComposedBundleSpec
	json.Unmarshal(spec, &s)
	composer := bundle.NewComposer(f.client, nil, "", f.operatorNamespace)
	result, err := composer.Compose(ctx, ns, &s, nil, nil)
	if err != nil {
		return nil, err
	}
	preview := &bus.BundleComposePreview{
		BundleYAML: result.BundleYAML,
		Missing:    result.Missing,
		Drifted:    result.Drifted,
		Warnings:   result.Warnings,
		Errors:     result.Errors,
	}
	if result.Materialized != nil {
		preview.JenkinsYAML = result.Materialized.JenkinsYAML
		preview.PluginsYAML = result.Materialized.PluginsYAML
		preview.ItemsYAML = result.Materialized.ItemsYAML
		preview.RbacYAML = result.Materialized.RbacYAML
		preview.PipelineTemplatesYAML = result.Materialized.PipelineTemplatesYAML
		if preview.PipelineTemplatesYAML != "" {
			var wrapper struct {
				Templates []pipelinetemplate.Document `yaml:"templates"`
			}
			if err := yaml.Unmarshal([]byte(preview.PipelineTemplatesYAML), &wrapper); err == nil {
				for _, doc := range wrapper.Templates {
					preview.PipelineTemplates = append(preview.PipelineTemplates, bus.PipelineTemplateSummary{
						Name:        doc.Metadata.Name,
						Version:     doc.Spec.Version,
						DisplayName: doc.Metadata.DisplayName,
						Description: doc.Metadata.Description,
					})
				}
			}
		}
		baseline, _ := pluginlock.Resolve("")
		if report, err := bundle.CheckPluginPins(preview.PluginsYAML, baseline); err == nil {
			preview.PinPreflight = bus.PinPreflightReport{Conflicts: []bus.PinConflict{}, Missing: []bus.MissingPlugin{}}
			for _, c := range report.Conflicts {
				preview.PinPreflight.Conflicts = append(preview.PinPreflight.Conflicts, bus.PinConflict{
					ArtifactID:    c.ArtifactID,
					BundleVersion: c.BundleVersion,
					SetVersion:    c.SetVersion,
				})
			}
			for _, m := range report.Missing {
				preview.PinPreflight.Missing = append(preview.PinPreflight.Missing, bus.MissingPlugin{
					ArtifactID:    m.ArtifactID,
					BundleVersion: m.BundleVersion,
				})
			}
		}
	}
	return preview, nil
}
func (f *fakeConfigBroodForTest) ListCatalogItems(ctx context.Context, cluster, ns string, filter CatalogItemFilter) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) GetCatalogItem(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) ListCatalogSources(ctx context.Context, cluster, ns string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) GetCatalogSource(ctx context.Context, cluster, ns, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) CreateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) UpdateCatalogSource(ctx context.Context, cluster, ns, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) DeleteCatalogSource(ctx context.Context, cluster, ns, name string) error {
	return nil
}
func (f *fakeConfigBroodForTest) SyncCatalogSource(ctx context.Context, cluster, ns, name string) error {
	return nil
}
func (f *fakeConfigBroodForTest) ListJenkinsRoles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) GetJenkinsRole(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) CreateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) UpdateJenkinsRole(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) DeleteJenkinsRole(ctx context.Context, cluster, name string) error {
	return nil
}
func (f *fakeConfigBroodForTest) ListJenkinsRoleBindings(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) GetJenkinsRoleBinding(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) CreateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) UpdateJenkinsRoleBinding(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) DeleteJenkinsRoleBinding(ctx context.Context, cluster, name string) error {
	return nil
}

func (f *fakeConfigBroodForTest) GetProvisioningDefaults(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) UpdateProvisioningDefaults(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *fakeConfigBroodForTest) ListVersionProfiles(ctx context.Context, cluster string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) GetVersionProfile(ctx context.Context, cluster, name string) (json.RawMessage, error) {
	return nil, nil
}
func (f *fakeConfigBroodForTest) CreateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *fakeConfigBroodForTest) UpdateVersionProfile(ctx context.Context, cluster, name string, obj json.RawMessage) (json.RawMessage, error) {
	return obj, nil
}
func (f *fakeConfigBroodForTest) DeleteVersionProfile(ctx context.Context, cluster, name string) error {
	return nil
}
func (f *fakeConfigBroodForTest) ViewVersionProfiles(ctx context.Context, cluster string) ([]bus.VersionProfileView, error) {
	return nil, nil
}

// --- Tests ---

func doPreview(t *testing.T, srv *Server, ns string, spec v1alpha1.ComposedBundleSpec) testComposedBundlePreview {
	t.Helper()
	body, _ := json.Marshal(spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/"+ns+"/preview", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{ns, "preview"})
	if w.Code != http.StatusOK {
		t.Fatalf("preview: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp testComposedBundlePreview
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	return resp
}

func TestPreviewComposedBundle_OperatorNamespaceParity(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(jcascItem("platform-catalog-theme", "varroa-system", "jenkins:\n  systemMessage: \"platform theme\"\n"))
	srv := newBundleParityServer(client, "varroa-system")

	specA := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "platform-catalog-theme"}},
		},
	}
	respA := doPreview(t, srv, "tenant-a", specA)
	if len(respA.Missing) != 0 {
		t.Fatalf("case A: expected no missing items, got %v", respA.Missing)
	}
	if !strings.Contains(respA.JenkinsYAML, "platform theme") {
		t.Fatalf("case A: expected operator-namespace item content in jenkinsYaml, got %q", respA.JenkinsYAML)
	}

	specB := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "platform-catalog-theme"}},
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "does-not-exist"}},
		},
	}
	respB := doPreview(t, srv, "tenant-a", specB)
	if len(respB.Missing) != 1 || respB.Missing[0] != "does-not-exist" {
		t.Fatalf("case B: expected missing == [does-not-exist], got %v", respB.Missing)
	}
	if !strings.Contains(respB.JenkinsYAML, "platform theme") {
		t.Fatalf("case B: resolvable item content should still render, got %q", respB.JenkinsYAML)
	}
}

func TestValidateComposedBundle_NamesMissingItems(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(jcascItem("platform-catalog-theme", "varroa-system", "jenkins:\n  systemMessage: \"ok\"\n"))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "platform-catalog-theme"}},
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "absent"}},
		},
	}
	body, _ := json.Marshal(spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/validate?namespace=tenant-a", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"validate"})
	if w.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode validate response: %v", err)
	}
}

func TestValidateComposedBundle_PinPreflightConflict(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(pluginItem("plugin-item", "plugins:\n  - artifactId: git\n    version: 999.999\n"))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "plugin-item"}},
		},
	}
	body, _ := json.Marshal(spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/validate?namespace=tenant-a", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"validate"})
	if w.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		PinPreflight bus.PinPreflightReport `json:"pinPreflight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode validate response: %v", err)
	}
	if len(resp.PinPreflight.Conflicts) != 1 || resp.PinPreflight.Conflicts[0].ArtifactID != "git" {
		t.Fatalf("pinPreflight.conflicts = %+v, want one conflict naming git", resp.PinPreflight.Conflicts)
	}
}

func TestValidateComposedBundle_PathSegmentRoute_PinPreflightConflict(t *testing.T) {
	// The /{ns}/validate route variant must report pinPreflight identically to
	// the /validate?namespace= variant covered above.
	client := newBundleTestClient()
	client.addItem(pluginItem("plugin-item", "plugins:\n  - artifactId: git\n    version: 999.999\n"))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "plugin-item"}},
		},
	}
	body, _ := json.Marshal(spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/tenant-a/validate", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"tenant-a", "validate"})
	if w.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		PinPreflight bus.PinPreflightReport `json:"pinPreflight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode validate response: %v", err)
	}
	if len(resp.PinPreflight.Conflicts) != 1 || resp.PinPreflight.Conflicts[0].ArtifactID != "git" {
		t.Fatalf("pinPreflight.conflicts = %+v, want one conflict naming git", resp.PinPreflight.Conflicts)
	}
}

func TestPreviewComposedBundle_PinPreflightWithoutSeparateRequest(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(pluginItem("plugin-item", "plugins:\n  - artifactId: git\n    version: 999.999\n"))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "plugin-item"}},
		},
	}
	resp := doPreview(t, srv, "tenant-a", spec)
	if len(resp.PinPreflight.Conflicts) != 1 || resp.PinPreflight.Conflicts[0].ArtifactID != "git" {
		t.Fatalf("pinPreflight.conflicts = %+v, want one conflict naming git, with no request field needed to opt in", resp.PinPreflight.Conflicts)
	}
}

func TestPreviewComposedBundle_PinPreflightAllClear(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(pluginItem("clean-plugin-item", "plugins:\n  - artifactId: git\n    version: 5.10.1\n"))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "clean-plugin-item"}},
		},
	}
	resp := doPreview(t, srv, "tenant-a", spec)
	if resp.PinPreflight.Conflicts == nil || len(resp.PinPreflight.Conflicts) != 0 {
		t.Errorf("pinPreflight.conflicts = %#v, want non-nil empty array", resp.PinPreflight.Conflicts)
	}
	if resp.PinPreflight.Missing == nil || len(resp.PinPreflight.Missing) != 0 {
		t.Errorf("pinPreflight.missing = %#v, want non-nil empty array", resp.PinPreflight.Missing)
	}
}

func TestCreateController_ComposedBundleRefResolution(t *testing.T) {
	client := newBundleTestClient()
	srv := newBundleParityServer(client, "varroa-system")

	// Pre-create a bundle in tenant-a.
	client.addBundle(&v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "app-bundle", Namespace: "team-a"},
		Spec:       v1alpha1.ComposedBundleSpec{},
	})

	// Verify we can get it via the cluster-scoped path.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/composedbundles/team-a/app-bundle", nil)
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"team-a", "app-bundle"})
	if w.Code != http.StatusOK {
		t.Fatalf("get bundle: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetComposedBundle_ProjectsSparseResolvedInputs(t *testing.T) {
	client := newBundleTestClient()
	client.addBundle(&v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "app-bundle", Namespace: "team-a"},
		Spec: v1alpha1.ComposedBundleSpec{Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "theme"}},
			{GitSource: &v1alpha1.GitBundleSource{RepoURL: "https://example.test/config.git", Path: "base"}},
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "jobs", Namespace: "shared"}},
		}},
		Status: v1alpha1.ComposedBundleStatus{
			InputSummary: []v1alpha1.InputSummaryEntry{
				{Kind: "itemRef", Type: "jcasc", Namespace: "varroa-system"},
			},
			ObservedRevisions: map[string]string{"theme": "sha-theme", "2": "sha-jobs"},
			MissingItems:      []string{"1"},
			DriftedItems:      []string{"2"},
		},
	})
	srv := newBundleParityServer(client, "varroa-system")
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/composedbundles/team-a/app-bundle", nil)
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"team-a", "app-bundle"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Status         map[string]interface{} `json:"status"`
		ResolvedInputs []resolvedBundleInput  `json:"resolvedInputs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.ResolvedInputs) != 3 {
		t.Fatalf("expected three stable entries, got %#v", response.ResolvedInputs)
	}
	if got := response.ResolvedInputs[0]; got.Index != 0 || got.Name != "theme" || got.Type != "jcasc" || got.Revision != "sha-theme" || got.Status != "Resolved" {
		t.Fatalf("unexpected first input: %#v", got)
	}
	if got := response.ResolvedInputs[1]; got.Index != 1 || got.Name != "https://example.test/config.git#base" || got.Status != "Missing" {
		t.Fatalf("unexpected sparse input: %#v", got)
	}
	if got := response.ResolvedInputs[2]; got.Index != 2 || got.Name != "jobs" || got.Namespace != "shared" || got.Status != "Drifted" {
		t.Fatalf("unexpected third input: %#v", got)
	}
	if _, leaked := response.Status["resolvedInputs"]; leaked {
		t.Fatal("resolvedInputs must remain a top-level BFF projection")
	}
}

// --- Task 7.3c: pipelineTemplates on preview and validate ---

const previewTemplateA = `apiVersion: varroa.dev/v1
kind: PipelineTemplate
metadata:
  name: svc-a
  displayName: Service A
  description: Builds service A
spec:
  version: "1.0.0"
  parameters: {}
  jenkinsfile: "pipeline {}"
`

const previewTemplateB = `apiVersion: varroa.dev/v1
kind: PipelineTemplate
metadata:
  name: svc-b
  displayName: Service B
  description: Builds service B
spec:
  version: "2.0.0"
  parameters: {}
  jenkinsfile: "pipeline {}"
`

// TestPreviewComposedBundle_PipelineTemplatesInInputOrder asserts the
// bundle-composition-surface delta's "A preview lists the delivered
// templates" scenario: two pipeline-template inputs surface as two
// entries, in input order, with their names and versions.
func TestPreviewComposedBundle_PipelineTemplatesInInputOrder(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(pipelineTemplateItem("template-b", previewTemplateB))
	client.addItem(pipelineTemplateItem("template-a", previewTemplateA))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "template-b"}},
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "template-a"}},
		},
	}
	resp := doPreview(t, srv, "tenant-a", spec)
	if len(resp.PipelineTemplates) != 2 {
		t.Fatalf("pipelineTemplates = %+v, want two entries", resp.PipelineTemplates)
	}
	if resp.PipelineTemplates[0].Name != "svc-b" || resp.PipelineTemplates[0].Version != "2.0.0" {
		t.Errorf("pipelineTemplates[0] = %+v, want svc-b@2.0.0 (input order)", resp.PipelineTemplates[0])
	}
	if resp.PipelineTemplates[1].Name != "svc-a" || resp.PipelineTemplates[1].Version != "1.0.0" {
		t.Errorf("pipelineTemplates[1] = %+v, want svc-a@1.0.0 (input order)", resp.PipelineTemplates[1])
	}
	if resp.PipelineTemplatesYAML == "" {
		t.Error("pipelineTemplatesYaml is empty, want the composed template content")
	}
}

// TestPreviewComposedBundle_NoTemplateIsEmptyArray asserts a bundle with
// no pipeline-template input answers pipelineTemplates: [], not null.
func TestPreviewComposedBundle_NoTemplateIsEmptyArray(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(jcascItem("theme", "varroa-system", "jenkins:\n  systemMessage: \"ok\"\n"))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "theme"}},
		},
	}
	resp := doPreview(t, srv, "tenant-a", spec)
	if resp.PipelineTemplates == nil || len(resp.PipelineTemplates) != 0 {
		t.Errorf("pipelineTemplates = %#v, want a non-nil empty array", resp.PipelineTemplates)
	}
}

// TestValidateComposedBundle_PipelineTemplatesInInputOrder asserts the
// same ordering rule reaches the /validate response, which is a
// hand-written map rather than bus.BundleComposePreview served directly.
func TestValidateComposedBundle_PipelineTemplatesInInputOrder(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(pipelineTemplateItem("template-b", previewTemplateB))
	client.addItem(pipelineTemplateItem("template-a", previewTemplateA))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "template-b"}},
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "template-a"}},
		},
	}
	body, _ := json.Marshal(spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/validate?namespace=tenant-a", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"validate"})
	if w.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		PipelineTemplates []bus.PipelineTemplateSummary `json:"pipelineTemplates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode validate response: %v", err)
	}
	if len(resp.PipelineTemplates) != 2 || resp.PipelineTemplates[0].Name != "svc-b" || resp.PipelineTemplates[1].Name != "svc-a" {
		t.Fatalf("pipelineTemplates = %+v, want [svc-b, svc-a] in input order", resp.PipelineTemplates)
	}
}

// TestValidateComposedBundle_PathSegmentRoute_NoTemplateIsEmptyArray
// asserts the /{ns}/validate route variant also answers pipelineTemplates:
// [] for a bundle with no pipeline-template input.
func TestValidateComposedBundle_PathSegmentRoute_NoTemplateIsEmptyArray(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(jcascItem("theme", "varroa-system", "jenkins:\n  systemMessage: \"ok\"\n"))
	srv := newBundleParityServer(client, "varroa-system")

	spec := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{
			{ItemRef: &v1alpha1.ComposedItemRef{Name: "theme"}},
		},
	}
	body, _ := json.Marshal(spec)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/clusters/core/composedbundles/tenant-a/validate", strings.NewReader(string(body)))
	srv.dispatchComposedBundles(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{"tenant-a", "validate"})
	if w.Code != http.StatusOK {
		t.Fatalf("validate: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		PipelineTemplates []bus.PipelineTemplateSummary `json:"pipelineTemplates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode validate response: %v", err)
	}
	if resp.PipelineTemplates == nil || len(resp.PipelineTemplates) != 0 {
		t.Errorf("pipelineTemplates = %#v, want a non-nil empty array", resp.PipelineTemplates)
	}
}
