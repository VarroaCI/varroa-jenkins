package controller

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// teamBundleFixture seeds a Team that refers to a ComposedBundle named
// payments-bundle in test-ns, and that bundle's content ConfigMap.
func teamBundleFixture(t *testing.T, contentRef string, cm map[string]string) *ConfigCRUD {
	t.Helper()
	fc := newFakeClient()
	fc.bundles["test-ns/payments-bundle"] = &v1alpha1.ComposedBundle{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-bundle", Namespace: "test-ns"},
		Status:     v1alpha1.ComposedBundleStatus{ContentRef: contentRef},
	}
	if cm != nil {
		fc.configMaps["test-ns/payments-content"] = cm
	}
	cc, st := newTestConfigCRUD(fc, "test-ns", nil)
	crdstore.MustSeed(st, &v1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: v1alpha1.TeamSpec{Pipelines: &v1alpha1.TeamPipelinesSpec{
			ComposedBundleRef: &v1alpha1.ComposedBundleRef{Name: "payments-bundle", Namespace: "test-ns"},
		}},
	})
	return cc
}

func decodeTeamContent(t *testing.T, data []byte) bus.TeamBundleContentResponse {
	t.Helper()
	var resp bus.TeamBundleContentResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return resp
}

func TestHandleBundlesTeamContent_ReturnsBundleAndFilteredContent(t *testing.T) {
	cc := teamBundleFixture(t, "payments-content", map[string]string{
		"items.yaml":              "items: []\n",
		"variables.yaml":          "a: b\n",
		"pipeline-templates.yaml": "templates: []\n",
		"dropped-item-keys.yaml":  "[]\n",
		"jenkins.yaml":            "jenkins: {}\n",
	})
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != "" {
		t.Fatalf("code = %q (%s), want none", resp.Code, resp.Error)
	}
	var cb v1alpha1.ComposedBundle
	if err := json.Unmarshal(resp.Bundle, &cb); err != nil || cb.Name != "payments-bundle" {
		t.Fatalf("bundle = %s (err %v), want payments-bundle", resp.Bundle, err)
	}
	want := []string{"items.yaml", "variables.yaml", "pipeline-templates.yaml", "dropped-item-keys.yaml"}
	if len(resp.Content) != len(want) {
		t.Fatalf("content keys = %v, want %v", resp.Content, want)
	}
	for _, k := range want {
		if _, ok := resp.Content[k]; !ok {
			t.Errorf("content lacks %q", k)
		}
	}
	if _, ok := resp.Content["jenkins.yaml"]; ok {
		t.Error("content holds jenkins.yaml, want it filtered out")
	}
}

func TestHandleBundlesTeamContent_MissingTeamAnswersNotFound(t *testing.T) {
	cc := teamBundleFixture(t, "payments-content", nil)
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "absent"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != bus.CodeNotFound {
		t.Fatalf("code = %q, want %q", resp.Code, bus.CodeNotFound)
	}
}

func TestHandleBundlesTeamContent_NoPipelinesOrNoRefAnswersEmpty(t *testing.T) {
	cc := teamBundleFixture(t, "payments-content", nil)
	crdstore.MustSeed(cc.Store.(*crdstore.Fake), &v1alpha1.Team{ObjectMeta: metav1.ObjectMeta{Name: "bare"}})
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "bare"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != "" || resp.Bundle != nil || resp.Content != nil {
		t.Fatalf("reply = %+v, want empty", resp)
	}
}

func TestHandleBundlesTeamContent_NoContentRefAnswersBundleOnly(t *testing.T) {
	cc := teamBundleFixture(t, "", map[string]string{"items.yaml": "items: []\n"})
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != "" || len(resp.Bundle) == 0 || resp.Content != nil {
		t.Fatalf("reply = %+v, want bundle only", resp)
	}
}

func TestHandleBundlesTeamContent_MissingConfigMapAnswersBundleOnly(t *testing.T) {
	cc := teamBundleFixture(t, "payments-content", nil)
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != "" || len(resp.Bundle) == 0 || resp.Content != nil {
		t.Fatalf("reply = %+v, want bundle only", resp)
	}
}

func TestHandleBundlesTeamContent_EmptyTeamIsInvalid(t *testing.T) {
	cc := teamBundleFixture(t, "payments-content", nil)
	req, _ := json.Marshal(bus.TeamBundleContentRequest{})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != bus.CodeInvalid {
		t.Fatalf("code = %q, want %q", resp.Code, bus.CodeInvalid)
	}
}

func TestHandleBundlesTeamContent_OversizeReplyIsRefused(t *testing.T) {
	cc := teamBundleFixture(t, "payments-content", map[string]string{
		"items.yaml": strings.Repeat("x", bus.ListPageBudget),
	})
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != bus.CodeInternal || resp.Bundle != nil {
		t.Fatalf("reply code = %q bundle = %d bytes, want code %q and no bundle", resp.Code, len(resp.Bundle), bus.CodeInternal)
	}
}
