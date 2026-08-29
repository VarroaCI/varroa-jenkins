package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/observability"
)

type observabilityIntentClient struct {
	controller.ResourceClient
	bundle *v1alpha1.ComposedBundle
	items  map[string]*v1alpha1.CatalogItem
}

func storeFromObsClient(c *observabilityIntentClient) *crdstore.Fake {
	st := crdstore.NewFake()
	if c.bundle != nil {
		crdstore.MustSeed(st, c.bundle)
	}
	for _, it := range c.items {
		crdstore.MustSeed(st, it)
	}
	return st
}

func (c *observabilityIntentClient) GetComposedBundleCRD(_ context.Context, name, namespace string) (*v1alpha1.ComposedBundle, error) {
	if c.bundle != nil && c.bundle.Name == name && c.bundle.Namespace == namespace {
		return c.bundle, nil
	}
	return nil, errNotFound("composedbundle", name)
}

func (c *observabilityIntentClient) ListComposedBundleCRDs(_ context.Context, _ string) ([]*v1alpha1.ComposedBundle, error) {
	if c.bundle == nil {
		return nil, nil
	}
	return []*v1alpha1.ComposedBundle{c.bundle}, nil
}

func (c *observabilityIntentClient) GetCatalogItemCRD(_ context.Context, name, namespace string) (*v1alpha1.CatalogItem, error) {
	item, ok := c.items[namespace+"/"+name]
	if !ok {
		return nil, errNotFound("catalogitem", name)
	}
	return item, nil
}

func TestObservabilityIntentAnnotationsFromBundleAndCatalogItems(t *testing.T) {
	client := &observabilityIntentClient{
		bundle: &v1alpha1.ComposedBundle{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bundle-a",
				Namespace: "varroa-system",
				Annotations: map[string]string{
					observability.AnnotationProviders:    "opentelemetry",
					observability.AnnotationCapabilities: "jenkins.traces.exporting",
				},
			},
			Spec: v1alpha1.ComposedBundleSpec{
				Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "metrics"}}},
			},
		},
		items: map[string]*v1alpha1.CatalogItem{
			"varroa-system/metrics": {
				ObjectMeta: metav1.ObjectMeta{
					Name:      "metrics",
					Namespace: "varroa-system",
					Annotations: map[string]string{
						observability.AnnotationProviders:    "prometheus",
						observability.AnnotationCapabilities: "jenkins.metrics.endpoint,unknown-capability",
					},
				},
			},
		},
	}
	// ConfigBrood is deliberately nil here: this exercises the legacy/nil
	// fallback path that must keep using the local typed client without
	// panicking.
	srv := &Server{deps: &Dependencies{Client: client, Store: storeFromObsClient(client)}}
	// The bundle lives in varroa-system while the controller is in dev; there
	// is no cross-namespace fallback, so the ref must name the namespace
	// explicitly (ref.Namespace || cr.Namespace, exact Get).
	controllerCR := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "dev"},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-a", Namespace: "varroa-system"}},
	}

	annotations := srv.observabilityIntentAnnotations(context.Background(), "", controllerCR)

	providers, capabilities, warnings := observability.UnionIntents(annotations)
	if len(warnings) != 1 {
		t.Fatalf("expected invalid-token warning, got %#v", warnings)
	}
	if len(providers) != 2 || !containsString(providers, "opentelemetry") || !containsString(providers, "prometheus") {
		t.Fatalf("unexpected providers: %#v", providers)
	}
	if len(capabilities) != 2 || !containsString(capabilities, "jenkins.traces.exporting") || !containsString(capabilities, "jenkins.metrics.endpoint") {
		t.Fatalf("unexpected capabilities: %#v", capabilities)
	}
}

// obsIntentBundle and obsIntentItem are the fixtures shared by the brood-routing
// tests: a bundle + a referenced catalog item, both carrying observability
// intent annotations.
func obsIntentBundle() *v1alpha1.ComposedBundle {
	return &v1alpha1.ComposedBundle{
		TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "ComposedBundle"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bundle-a",
			Namespace: "varroa-system",
			Annotations: map[string]string{
				observability.AnnotationProviders:    "opentelemetry",
				observability.AnnotationCapabilities: "jenkins.traces.exporting",
			},
		},
		Spec: v1alpha1.ComposedBundleSpec{
			Inputs: []v1alpha1.ComposedInput{{ItemRef: &v1alpha1.ComposedItemRef{Name: "metrics"}}},
		},
	}
}

func obsIntentItem() *v1alpha1.CatalogItem {
	return &v1alpha1.CatalogItem{
		TypeMeta: metav1.TypeMeta{APIVersion: "varroa.dev/v1alpha1", Kind: "CatalogItem"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "metrics",
			Namespace: "varroa-system",
			Annotations: map[string]string{
				observability.AnnotationProviders:    "prometheus",
				observability.AnnotationCapabilities: "jenkins.metrics.endpoint",
			},
		},
	}
}

func obsIntentController() *v1alpha1.Controller {
	return &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "ctrl", Namespace: "dev"},
		Spec:       v1alpha1.ControllerSpec{ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "bundle-a", Namespace: "varroa-system"}},
	}
}

func assertObsIntentUnion(t *testing.T, annotations map[string]string) {
	t.Helper()
	providers, capabilities, warnings := observability.UnionIntents(annotations)
	if len(warnings) != 0 {
		t.Fatalf("unexpected intent warnings: %#v", warnings)
	}
	if len(providers) != 2 || !containsString(providers, "opentelemetry") || !containsString(providers, "prometheus") {
		t.Fatalf("unexpected providers: %#v", providers)
	}
	if len(capabilities) != 2 || !containsString(capabilities, "jenkins.traces.exporting") || !containsString(capabilities, "jenkins.metrics.endpoint") {
		t.Fatalf("unexpected capabilities: %#v", capabilities)
	}
}

// TestObservabilityIntentAnnotationsCoreViaLocalPath asserts that for a
// controller on the BFF's own (core) cluster the ConfigBrood local branch
// resolves the bundle+item through the typed core client and never touches the
// bus.
func TestObservabilityIntentAnnotationsCoreViaLocalPath(t *testing.T) {
	client := &observabilityIntentClient{
		bundle: obsIntentBundle(),
		items:  map[string]*v1alpha1.CatalogItem{"varroa-system/metrics": obsIntentItem()},
	}
	brood := &busConfigBrood{
		localCluster: "core",
		client:       client,
		store:        storeFromObsClient(client),
		logger:       slog.Default(),
		request: func(subject string, _ []byte, _ time.Duration) ([]byte, error) {
			t.Errorf("local cluster must not issue a bus request, got %q", subject)
			return nil, fmt.Errorf("unexpected bus request")
		},
	}
	rec := &recordingLogHandler{}
	srv := &Server{deps: &Dependencies{Client: client, Store: storeFromObsClient(client), ConfigBrood: brood, Logger: slog.New(rec)}}

	annotations := srv.observabilityIntentAnnotations(context.Background(), "core", obsIntentController())

	assertObsIntentUnion(t, annotations)
	if got := rec.count("observability bundle lookup failed"); got != 0 {
		t.Fatalf("expected no bundle-lookup-failed warning on core path, got %d", got)
	}
}

// TestObservabilityIntentAnnotationsRemoteViaBus asserts that for a controller
// on a remote (hive) cluster the ConfigBrood remote branch resolves the
// bundle+item over the bus (never the local client) and logs no lookup-failed
// warning.
func TestObservabilityIntentAnnotationsRemoteViaBus(t *testing.T) {
	bundleJSON, err := json.Marshal(obsIntentBundle())
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	itemJSON, err := json.Marshal(obsIntentItem())
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	brood := &busConfigBrood{
		localCluster: "core",
		client:       nil, // remote path must never dereference the local client
		logger:       slog.Default(),
		request: func(subject string, _ []byte, _ time.Duration) ([]byte, error) {
			switch {
			case strings.Contains(subject, ".bundles.get"):
				return json.Marshal(bus.ConfigGetResponse{Item: bundleJSON})
			case strings.Contains(subject, ".catalog.itemget"):
				return json.Marshal(bus.ConfigGetResponse{Item: itemJSON})
			default:
				return nil, fmt.Errorf("unexpected subject %q", subject)
			}
		},
	}
	rec := &recordingLogHandler{}
	srv := &Server{deps: &Dependencies{Client: nil, ConfigBrood: brood, Logger: slog.New(rec)}}

	annotations := srv.observabilityIntentAnnotations(context.Background(), "hive", obsIntentController())

	assertObsIntentUnion(t, annotations)
	if got := rec.count("observability bundle lookup failed"); got != 0 {
		t.Fatalf("expected no bundle-lookup-failed warning on remote path, got %d", got)
	}
	if got := rec.count("observability catalog item lookup failed"); got != 0 {
		t.Fatalf("expected no item-lookup-failed warning on remote path, got %d", got)
	}
}

// recordingLogHandler is a minimal slog.Handler that records emitted messages so
// tests can assert whether a specific warning fired.
type recordingLogHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingLogHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.messages {
		if m == msg {
			n++
		}
	}
	return n
}

func errNotFound(kind, name string) error {
	return &notFoundError{kind: kind, name: name}
}

type notFoundError struct {
	kind string
	name string
}

func (e *notFoundError) Error() string { return e.kind + " " + e.name + " not found" }
