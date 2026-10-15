package controller

import (
	"context"
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

// placedTeamHoldFixture is teamBundleFixture with the Team placed on cell-1.
// cell-1 carries the given status. A nil status leaves cell-1 absent.
func placedTeamHoldFixture(t *testing.T, cellStatus *v1alpha1.CellStatus) *ConfigCRUD {
	t.Helper()
	return placedTeamHoldFixtureWithContent(t, cellStatus, map[string]string{"items.yaml": "items: []\n"})
}

// placedTeamHoldFixtureWithContent is placedTeamHoldFixture with the
// bundle's content ConfigMap set to content. A nil content leaves it absent.
func placedTeamHoldFixtureWithContent(t *testing.T, cellStatus *v1alpha1.CellStatus, content map[string]string) *ConfigCRUD {
	t.Helper()
	cc := teamBundleFixture(t, "payments-content", content)
	st := cc.Store.(*crdstore.Fake)
	team, err := crdstore.Get[v1alpha1.Team](context.Background(), st, "payments", "")
	if err != nil {
		t.Fatalf("get team: %v", err)
	}
	team.Status.Placement = &v1alpha1.TeamPlacementStatus{Cell: "cell-1", CellNamespace: "test-ns"}
	if err := crdstore.Update(context.Background(), st, team); err != nil {
		t.Fatalf("place team: %v", err)
	}
	if cellStatus != nil {
		crdstore.MustSeed(st, &v1alpha1.Controller{
			ObjectMeta: metav1.ObjectMeta{Name: "cell-1", Namespace: "test-ns"},
			Status:     v1alpha1.ControllerStatus{Cell: cellStatus},
		})
	}
	return cc
}

func itemsHeldStatus(tenant string) *v1alpha1.CellStatus {
	return &v1alpha1.CellStatus{TenantWarnings: []v1alpha1.TenantWarning{{
		Tenant: tenant, Reason: v1alpha1.TeamReasonItemsHeld, Message: "tenant folder held: plugin not pinned",
	}}}
}

func TestHandleBundlesTeamContent_HeldTenantAnswersHeld(t *testing.T) {
	cc := placedTeamHoldFixture(t, itemsHeldStatus("payments"))
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != "" {
		t.Fatalf("code = %q (%s), want none", resp.Code, resp.Error)
	}
	if !resp.Held {
		t.Fatal("held = false, want true: the cell records an ItemsHeld hold")
	}
}

// TestHandleBundlesTeamContent_HeldTenantWithoutContentAnswersHeld asserts the
// hold reaches the reply even when the content ConfigMap is absent, since the
// reply then carries no items and an early return would drop the flag.
func TestHandleBundlesTeamContent_HeldTenantWithoutContentAnswersHeld(t *testing.T) {
	cc := placedTeamHoldFixtureWithContent(t, itemsHeldStatus("payments"), nil)
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != "" || len(resp.Bundle) == 0 {
		t.Fatalf("reply = %+v, want the bundle without content", resp)
	}
	if !resp.Held {
		t.Fatal("held = false, want true: the cell records an ItemsHeld hold")
	}
}

func TestHandleBundlesTeamContent_UnheldTenantAnswersNotHeld(t *testing.T) {
	cc := placedTeamHoldFixture(t, &v1alpha1.CellStatus{})
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Held {
		t.Fatal("held = true, want false: the cell records no hold")
	}
	if resp.Content["items.yaml"] == "" {
		t.Fatal("content lacks items.yaml, want the tenant's items")
	}
}

func TestHandleBundlesTeamContent_MissingCellAnswersNotHeld(t *testing.T) {
	cc := placedTeamHoldFixture(t, nil)
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Code != "" || resp.Held {
		t.Fatalf("reply = %+v, want no code and not held", resp)
	}
}

func TestHandleBundlesTeamContent_UnplacedTeamAnswersNotHeld(t *testing.T) {
	cc := teamBundleFixture(t, "payments-content", map[string]string{"items.yaml": "items: []\n"})
	req, _ := json.Marshal(bus.TeamBundleContentRequest{Team: "payments"})
	resp := decodeTeamContent(t, cc.HandleBundlesTeamContent(req))
	if resp.Held {
		t.Fatal("held = true, want false: the Team has no placement")
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
