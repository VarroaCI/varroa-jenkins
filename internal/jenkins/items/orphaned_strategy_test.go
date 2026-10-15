package items

import (
	"strings"
	"testing"
)

func TestGenerateConfigXMLMultibranchOrphanedItemStrategy(t *testing.T) {
	base := func(s *OrphanedStrategy) Item {
		return Item{Kind: "multibranch", Name: "branches", Description: "d",
			SourcesList:          []BranchSourceEntry{{Git: &GitSCMSource{Remote: "https://example.com/r.git"}}},
			OrphanedItemStrategy: s,
		}
	}
	tests := []struct {
		name     string
		strategy *OrphanedStrategy
		want     []string
		wantNot  []string
	}{
		{"absent key renders no element", nil,
			nil, []string{"orphanedItemStrategy"}},
		{"daysToKeep only", &OrphanedStrategy{DaysToKeep: 7},
			[]string{"<pruneDeadBranches>true</pruneDeadBranches>", "<daysToKeep>7</daysToKeep>", "<numToKeep>-1</numToKeep>"}, nil},
		{"empty strategy maps zero limits to -1", &OrphanedStrategy{},
			[]string{"<pruneDeadBranches>true</pruneDeadBranches>", "<daysToKeep>-1</daysToKeep>", "<numToKeep>-1</numToKeep>"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xml, err := GenerateConfigXML(base(tt.strategy))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(xml, w) {
					t.Errorf("XML missing %q:\n%s", w, xml)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(xml, w) {
					t.Errorf("XML unexpectedly contains %q", w)
				}
			}
		})
	}
	t.Run("element sits between description and sources", func(t *testing.T) {
		xml, err := GenerateConfigXML(base(&OrphanedStrategy{DaysToKeep: 7}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		start := strings.Index(xml, "<orphanedItemStrategy")
		if start < 0 {
			t.Fatalf("XML has no orphanedItemStrategy element:\n%s", xml)
		}
		if d := strings.Index(xml, "</description>"); d < 0 || start < d {
			t.Errorf("orphanedItemStrategy is not after </description>")
		}
		if s := strings.Index(xml, "<sources"); s < 0 || start > s {
			t.Errorf("orphanedItemStrategy is not before <sources")
		}
	})
}
