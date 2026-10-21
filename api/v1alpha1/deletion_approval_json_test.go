package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestDeletionApprovalJSONUsesCommandId checks that the approval and imperative
// status types serialize the command key as commandId, the spelling the BFF,
// OpenAPI, and frontend use.
func TestDeletionApprovalJSONUsesCommandId(t *testing.T) {
	cases := map[string]any{
		"DeletionApproval":   DeletionApproval{CommandID: "c1", ApprovedAt: metav1.Now()},
		"RestartDrainStatus": RestartDrainStatus{CommandID: "c1"},
		"ImperativeResult":   ImperativeResult{CommandID: "c1"},
	}
	for name, v := range cases {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		out := string(raw)
		if !strings.Contains(out, `"commandId":"c1"`) {
			t.Errorf("%s: output %s lacks commandId", name, out)
		}
		if strings.Contains(out, `"commandID"`) {
			t.Errorf("%s: output %s still has commandID", name, out)
		}
	}
}
