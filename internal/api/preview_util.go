package api

import "github.com/varroaci/varroa-jenkins/internal/bus"

// normalizePreview fills nil slice fields so they serialize as [] not null.
func normalizePreview(p *bus.BundleComposePreview) {
	if p == nil {
		return
	}
	if p.Missing == nil {
		p.Missing = []string{}
	}
	if p.Drifted == nil {
		p.Drifted = []string{}
	}
	if p.Warnings == nil {
		p.Warnings = []string{}
	}
	if p.UnresolvedVariables == nil {
		p.UnresolvedVariables = []string{}
	}
	if p.Errors == nil {
		p.Errors = []string{}
	}
}
