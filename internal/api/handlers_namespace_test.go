package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/observability"
)

// Task 4.1: the /catalogitems envelope carries the configured operator namespace.
func TestListCatalogItems_OperatorNamespaceEnvelope(t *testing.T) {
	client := newBundleTestClient()
	fakeBrood := &scopeTestConfigBrood{operatorNs: "varroa-system"}
	srv := NewServer(&Dependencies{
		Client:            client,
		Store:             storeFromBundleClient(client),
		Authorizer:        adminTestAuthorizer(),
		ConfigBrood:       fakeBrood,
		OperatorNamespace: "varroa-system",
		Logger:            slog.Default(),
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/clusters/core/catalogitems", nil)
	srv.dispatchCatalogItems(w, req.WithContext(contextWithClaims(req.Context(), adminClaims)), "core", []string{})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Items             []json.RawMessage `json:"items"`
		OperatorNamespace string            `json:"operatorNamespace"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OperatorNamespace != "varroa-system" {
		t.Fatalf("operatorNamespace = %q, want varroa-system", resp.OperatorNamespace)
	}
}

func TestListCatalogItems_LocalRemoteSummaryParity(t *testing.T) {
	client := newBundleTestClient()
	item := &v1alpha1.CatalogItem{
		ObjectMeta: metav1.ObjectMeta{Name: "item1", Namespace: "team-a"},
		Spec: v1alpha1.CatalogItemSpec{
			SourceRef:   "source-a",
			Type:        v1alpha1.CatalogItemJCasC,
			DisplayName: "Display",
			Version:     "1.0.0",
			Description: "desc",
			Tags:        []string{"tag-a"},
			Path:        "detail-only.yaml",
		},
		Status: v1alpha1.CatalogItemStatus{Valid: true, Message: "ok", ContentHash: "hash-a", Content: "content"},
	}
	client.addItem(item)
	st := crdstore.NewFake()
	crdstore.MustSeed(st, item)

	brood := &busConfigBrood{
		localCluster: "core",
		client:       client,
		store:        st,
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			if subject != bus.OperatorCatalogSubject("member", "itemlist") {
				t.Fatalf("unexpected subject %q", subject)
			}
			raw, err := json.Marshal(controller.ProjectCatalogItemSummary(item))
			if err != nil {
				t.Fatal(err)
			}
			return json.Marshal(bus.ConfigListResponse{
				Items:             []json.RawMessage{raw},
				OperatorNamespace: "varroa-system",
			})
		},
	}

	localItems, localOperatorNs, err := brood.ListCatalogItems(context.Background(), "core", "team-a", CatalogItemFilter{})
	if err != nil {
		t.Fatalf("local list: %v", err)
	}
	remoteItems, remoteOperatorNs, err := brood.ListCatalogItems(context.Background(), "member", "team-a", CatalogItemFilter{})
	if err != nil {
		t.Fatalf("remote list: %v", err)
	}
	if localOperatorNs != "" {
		t.Fatalf("local operatorNamespace = %q, want empty", localOperatorNs)
	}
	if remoteOperatorNs != "varroa-system" {
		t.Fatalf("remote operatorNamespace = %q, want varroa-system", remoteOperatorNs)
	}
	if len(localItems) != 1 || len(remoteItems) != 1 {
		t.Fatalf("items len local=%d remote=%d", len(localItems), len(remoteItems))
	}
	if string(localItems[0]) != string(remoteItems[0]) {
		t.Fatalf("local/remote summary mismatch:\nlocal=%s\nremote=%s", localItems[0], remoteItems[0])
	}
	var raw map[string]any
	if err := json.Unmarshal(localItems[0], &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["status"]; ok {
		t.Fatalf("summary contains status: %v", raw)
	}
	if _, ok := raw["spec"]; ok {
		t.Fatalf("summary contains spec: %v", raw)
	}
}

func TestListCatalogItems_Filter(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"name":"groovy-one","type":"groovy","sourceRef":"source-a","description":"Deploy app","tags":["release"]}`),
		json.RawMessage(`{"name":"yaml-one","type":"jcasc","sourceRef":"source-a","description":"Other","tags":["Groovy"]}`),
		json.RawMessage(`{"name":"groovy-two","type":"groovy","sourceRef":"source-b","description":"Other","tags":["misc"]}`),
	}
	brood := &busConfigBrood{
		localCluster: "core",
		client:       newBundleTestClient(),
		logger:       slog.Default(),
		request: func(subject string, data []byte, timeout time.Duration) ([]byte, error) {
			return json.Marshal(bus.ConfigListResponse{Items: items})
		},
	}

	tests := []struct {
		name   string
		filter CatalogItemFilter
		want   int
	}{
		{name: "type", filter: CatalogItemFilter{Type: "groovy"}, want: 2},
		{name: "source", filter: CatalogItemFilter{Source: "source-b"}, want: 1},
		{name: "query case insensitive", filter: CatalogItemFilter{Q: "DEPLOY"}, want: 1},
		{name: "query matches tag", filter: CatalogItemFilter{Q: "RELEASE"}, want: 1},
		{name: "combined and", filter: CatalogItemFilter{Type: "groovy", Source: "source-a", Q: "release"}, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := brood.ListCatalogItems(context.Background(), "member", "team-a", tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.want {
				t.Fatalf("got %d items, want %d", len(got), tt.want)
			}
		})
	}

	brood.request = func(string, []byte, time.Duration) ([]byte, error) {
		return []byte(`{"items":[{"name":"groovy-one","type":"groovy"},[]]}`), nil
	}
	if _, _, err := brood.ListCatalogItems(context.Background(), "member", "team-a", CatalogItemFilter{Q: "x"}); err == nil {
		t.Fatal("filtered malformed summary should fail")
	}
	got, _, err := brood.ListCatalogItems(context.Background(), "member", "team-a", CatalogItemFilter{})
	if err != nil || len(got) != 2 {
		t.Fatalf("unfiltered malformed response got %d items, err %v", len(got), err)
	}
}

// Task 4.2: observabilityIntentAnnotations honors an explicit itemRef.namespace and
// does NOT consult the operator namespace for an unset itemRef.
func TestObservabilityIntentAnnotations_ItemRefNamespace(t *testing.T) {
	itemWithProvider := func(name, ns, provider string) *v1alpha1.CatalogItem {
		it := jcascItem(name, ns, "jenkins: {}\n")
		it.Annotations = map[string]string{observability.AnnotationProviders: provider}
		return it
	}

	t.Run("explicit itemRef namespace is used", func(t *testing.T) {
		client := newBundleTestClient()
		client.addBundle(&v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "obs-bundle", Namespace: "team-a"},
			Spec: v1alpha1.ComposedBundleSpec{
				Inputs: []v1alpha1.ComposedInput{
					{ItemRef: &v1alpha1.ComposedItemRef{Name: "theme", Namespace: "team-b"}},
				},
			},
		})
		client.addItem(itemWithProvider("theme", "team-b", "prometheus"))
		srv := NewServer(&Dependencies{Client: client, Store: storeFromBundleClient(client), OperatorNamespace: "varroa-system", Logger: slog.Default()})
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "obs-bundle"}},
		}
		ann := srv.observabilityIntentAnnotations(context.Background(), "", cr)
		if ann[observability.AnnotationProviders] != "prometheus" {
			t.Fatalf("expected provider from team-b item, got %v", ann)
		}
	})

	t.Run("unset itemRef does not consult operator namespace", func(t *testing.T) {
		client := newBundleTestClient()
		client.addBundle(&v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{Name: "obs-bundle", Namespace: "team-a"},
			Spec: v1alpha1.ComposedBundleSpec{
				Inputs: []v1alpha1.ComposedInput{
					{ItemRef: &v1alpha1.ComposedItemRef{Name: "theme"}},
				},
			},
		})
		// Same-named item only in the operator namespace — must NOT be consulted.
		client.addItem(itemWithProvider("theme", "varroa-system", "prometheus"))
		srv := NewServer(&Dependencies{Client: client, Store: storeFromBundleClient(client), OperatorNamespace: "varroa-system", Logger: slog.Default()})
		cr := &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "team-a"},
			Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "obs-bundle"}},
		}
		ann := srv.observabilityIntentAnnotations(context.Background(), "", cr)
		if _, ok := ann[observability.AnnotationProviders]; ok {
			t.Fatalf("operator-namespace item must not be consulted for an unset ref, got %v", ann)
		}
	})
}

// Task 4.3: preview/validate parity for explicit-namespace itemRefs (no handler
// changes — parity is inherited from the composer).
func TestPreviewValidate_ExplicitRefParity(t *testing.T) {
	client := newBundleTestClient()
	client.addItem(jcascItem("shared", "team-b", "jenkins:\n  systemMessage: \"from team-b\"\n"))
	srv := newBundleParityServer(client, "varroa-system")

	// (a) explicit third-namespace ref resolves and content appears — the preview
	// namespace is irrelevant to an explicit ref, so exercise a different tenant here.
	specHit := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "shared", Namespace: "team-b"}}},
	}
	respHit := doPreview(t, srv, "tenant-b", specHit)
	if len(respHit.Missing) != 0 {
		t.Fatalf("expected no missing, got %v", respHit.Missing)
	}
	if !strings.Contains(respHit.JenkinsYAML, "from team-b") {
		t.Fatalf("expected team-b content, got %q", respHit.JenkinsYAML)
	}

	// (b) explicit miss → namespace-qualified missing on preview + validate.
	specMiss := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "theme", Namespace: "team-b"}}},
	}
	respMiss := doPreview(t, srv, "tenant-a", specMiss)
	if len(respMiss.Missing) != 1 || respMiss.Missing[0] != "team-b/theme" {
		t.Fatalf("expected preview missing [team-b/theme], got %v", respMiss.Missing)
	}

	// (c) shadowing spec (row 4) carries the warning on preview.
	client.addItem(jcascItem("dup", "tenant-a", "jenkins:\n  systemMessage: \"local\"\n"))
	client.addItem(jcascItem("dup", "varroa-system", "jenkins:\n  systemMessage: \"platform\"\n"))
	specShadow := v1alpha1.ComposedBundleSpec{
		Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "dup"}}},
	}
	respShadow := doPreview(t, srv, "tenant-a", specShadow)
	warnJoined := strings.Join(respShadow.Warnings, " ")
	if !strings.Contains(warnJoined, "tenant-a/dup") || !strings.Contains(warnJoined, "varroa-system/dup") {
		t.Fatalf("expected shadow warning naming both namespaces, got %v", respShadow.Warnings)
	}
}
