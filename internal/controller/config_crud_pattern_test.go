package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// A Pattern scope that Java cannot compile is refused on create and on update,
// and the stored binding keeps its last valid pattern.
func TestHandleBindingsRefuseUncompilablePattern(t *testing.T) {
	fc := newFakeClient()
	cr, crStore := newTestConfigCRUD(fc, "", nil)

	bad := func(pattern string) []byte {
		return jsonMarshal(t, v1alpha1.JenkinsRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "jrb1"},
			Spec: v1alpha1.JenkinsRoleBindingSpec{
				RoleRef:      "builder",
				Subjects:     []v1alpha1.SubjectRef{{Kind: "Group", Name: "devs"}},
				JenkinsScope: &v1alpha1.JenkinsScope{Type: "Pattern", Pattern: pattern},
			},
		})
	}

	var appResp bus.ConfigApplyResponse
	jsonUnmarshal(t, cr.HandleBindingsCreate(jsonMarshal(t, bus.ConfigApplyRequest{Name: "jrb1", Object: bad("*-prod")})), &appResp)
	if appResp.Code != bus.CodeInvalid || !strings.Contains(appResp.Error, "is not a valid Java regex") {
		t.Fatalf("create with glob: code %q error %q, want CodeInvalid with the Java regex message", appResp.Code, appResp.Error)
	}
	if got, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), crStore, "jrb1", ""); err == nil && got != nil {
		t.Fatalf("refused create stored a binding: %+v", got)
	}

	appResp = bus.ConfigApplyResponse{}
	jsonUnmarshal(t, cr.HandleBindingsCreate(jsonMarshal(t, bus.ConfigApplyRequest{Name: "jrb1", Object: bad("payments/.*")})), &appResp)
	if appResp.Code != "" {
		t.Fatalf("create with valid pattern: code %q error %q", appResp.Code, appResp.Error)
	}

	appResp = bus.ConfigApplyResponse{}
	jsonUnmarshal(t, cr.HandleBindingsUpdate(jsonMarshal(t, bus.ConfigApplyRequest{Name: "jrb1", Object: bad("(payments")})), &appResp)
	if appResp.Code != bus.CodeInvalid {
		t.Fatalf("update with unclosed group: code %q error %q, want CodeInvalid", appResp.Code, appResp.Error)
	}
	stored, err := crdstore.Get[v1alpha1.JenkinsRoleBinding](context.Background(), crStore, "jrb1", "")
	if err != nil {
		t.Fatalf("get stored binding: %v", err)
	}
	if stored.Spec.JenkinsScope == nil || stored.Spec.JenkinsScope.Pattern != "payments/.*" {
		t.Fatalf("stored pattern = %+v, want payments/.*", stored.Spec.JenkinsScope)
	}
}
