// Package lint runs policy rules over an items manifest. It is a seam
// separate from items.Parse: a non-cell controller must keep accepting
// every item shape it accepts today, so no rule runs there. The cell rule
// set runs only in the operator, only for a cell.
package lint

import "github.com/varroaci/varroa-jenkins/internal/jenkins/items"

// Finding is one policy violation found in a manifest.
type Finding struct {
	Rule    string // the rule id
	Path    string // the item path the finding is about
	Message string
}

// Rule checks one item against a policy and returns every finding.
type Rule interface {
	ID() string
	Check(item items.Item, path string) []Finding
}

// Run applies every rule to every item of the flattened manifest.
func Run(m *items.Manifest, rules []Rule) []Finding {
	flattened := m.Flatten()
	findings := make([]Finding, 0, len(flattened))
	for _, ip := range flattened {
		for _, r := range rules {
			findings = append(findings, r.Check(ip.Item, ip.Path)...)
		}
	}
	return findings
}
