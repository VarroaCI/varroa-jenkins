package recordstore

import (
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// TestCellRollCauseShapeAdmitsTheOperatorReasons asserts that the shape
// admits each reason the operator's cell readiness rule writes on a
// cell.rolled row, and refuses text that is not one reason word.
func TestCellRollCauseShapeAdmitsTheOperatorReasons(t *testing.T) {
	for _, reason := range []string{v1alpha1.ReasonMiteUnreachable, v1alpha1.ReasonNotProvisioned} {
		if !cellRollCauseShape.MatchString(reason) {
			t.Errorf("the shape refuses the operator reason %q", reason)
		}
	}
	for _, text := range []string{
		"",
		"rolled the cell pod: MiteUnreachable for 5m0s",
		"mite-unreachable",
		"MiteUnreachable; node slab1",
		strings.Repeat("A", 65),
	} {
		if cellRollCauseShape.MatchString(text) {
			t.Errorf("the shape admits %q", text)
		}
	}
}
