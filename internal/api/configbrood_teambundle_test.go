package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

func TestConfigBrood_GetTeamBundleContentDecodesReply(t *testing.T) {
	rawBundle, err := json.Marshal(&v1alpha1.ComposedBundle{ObjectMeta: metav1.ObjectMeta{Name: "payments-bundle", Namespace: "cells"}})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := json.Marshal(bus.TeamBundleContentResponse{
		Bundle:  rawBundle,
		Content: map[string]string{"items.yaml": "items: []\n"},
		Held:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f, calls := cellPoolConfigBrood(func(string, []byte) ([]byte, error) { return reply, nil })

	cb, content, held, err := f.GetTeamBundleContent(context.Background(), "member-a", "payments")
	if err != nil {
		t.Fatalf("GetTeamBundleContent: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].subject != "operator.member-a.bundles.teamcontent" {
		t.Fatalf("calls = %+v, want one on operator.member-a.bundles.teamcontent", *calls)
	}
	if got := string((*calls)[0].data); got != `{"team":"payments"}` {
		t.Fatalf("request body = %s, want {\"team\":\"payments\"}", got)
	}
	if cb == nil || cb.Name != "payments-bundle" || cb.Namespace != "cells" {
		t.Fatalf("bundle = %+v, want payments-bundle in cells", cb)
	}
	if content["items.yaml"] != "items: []\n" {
		t.Fatalf("content = %v, want items.yaml", content)
	}
	if !held {
		t.Fatal("held = false, want the member's hold")
	}
}

func TestConfigBrood_GetTeamBundleContentRefusesLocal(t *testing.T) {
	f, calls := cellPoolConfigBrood(func(string, []byte) ([]byte, error) {
		t.Fatal("a local read must not reach the bus")
		return nil, nil
	})
	_, _, _, err := f.GetTeamBundleContent(context.Background(), "core", "payments")
	var be *BroodError
	if !errors.As(err, &be) {
		t.Fatalf("err = %v, want a *BroodError", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %d, want none", len(*calls))
	}
}

func TestConfigBrood_GetTeamBundleContentMapsCode(t *testing.T) {
	reply, err := json.Marshal(bus.TeamBundleContentResponse{Code: bus.CodeNotFound, Error: "team not found"})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := cellPoolConfigBrood(func(string, []byte) ([]byte, error) { return reply, nil })
	_, _, _, err = f.GetTeamBundleContent(context.Background(), "member-a", "absent")
	var be *BroodError
	if !errors.As(err, &be) || be.Code != bus.CodeNotFound {
		t.Fatalf("err = %v, want a *BroodError with code %q", err, bus.CodeNotFound)
	}
}
