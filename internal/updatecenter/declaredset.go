package updatecenter

import (
	"os"
	"slices"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter/ucmeta"
)

// DeclaredSet is the operator's view of every plugin any JenkinsVersionProfile or
// ComposedBundle declares. A plugin may appear at more than one version:
// buildDeclaredSet does not deduplicate across sources, and a multi-version
// declaration is still a declaration.
//
// The update-center service has no Kubernetes client. It learns the declared set
// through the operator-written ConfigMap `varroa-updatecenter-metadata`, key
// `declared-plugins`, mounted as a file and re-read per request — the same channel
// that already delivers `lts-metadata-urls`.
type DeclaredSet map[string][]string // name -> versions, in file order

// Declared reports whether the set names the plugin at any version.
func (d DeclaredSet) Declared(name string) bool { return len(d[name]) > 0 }

// Highest returns the highest pinned version for name by pluginver ordering.
// An unpinned version (see ucmeta.IsLatest) satisfies any minimum, so it never
// sets the highest pin. Highest reports false when name has no pinned version.
func (d DeclaredSet) Highest(name string) (string, bool) {
	var best string
	found := false
	for _, v := range d[name] {
		if ucmeta.IsLatest(v) {
			continue
		}
		if !found || pluginver.Compare(v, best) > 0 {
			best = v
			found = true
		}
	}
	return best, found
}

// Unpinned reports whether the set names the plugin and no version of it is
// pinned. Such a plugin is declared only at latest.
func (d DeclaredSet) Unpinned(name string) bool {
	if !d.Declared(name) {
		return false
	}
	_, pinned := d.Highest(name)
	return !pinned
}

// ReadDeclaredPlugins parses the newline-delimited "name@version" declared-plugins
// file.
//
// It deliberately distinguishes "the operator says nothing is declared" (ok=true,
// empty set) from "the file is missing or unreadable" (ok=false). Conflating them
// would be a correctness bug, not a cosmetic one: an empty set means every
// dependency resolves against upstream, which for a cluster that *does* pin its
// plugins would fetch unpinned versions for plugins that are in fact locked. The
// caller rejects an ok=false read rather than planning against it.
//
// An empty path yields (empty, false): a service that was never told where the file
// is cannot claim to know what is declared.
func ReadDeclaredPlugins(path string) (DeclaredSet, bool) {
	if path == "" {
		return DeclaredSet{}, false
	}
	data, err := os.ReadFile(path) // #nosec G304 -- operator-supplied mount path
	if err != nil {
		return DeclaredSet{}, false
	}
	set := DeclaredSet{}
	for _, line := range strings.Split(string(data), "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		// Split on the LAST '@': a plugin short name cannot contain '@', but this
		// keeps the parse unambiguous regardless.
		at := strings.LastIndex(entry, "@")
		if at <= 0 || at == len(entry)-1 {
			continue // malformed line — skip it rather than fail the whole read
		}
		name, version := entry[:at], entry[at+1:]
		if slices.Contains(set[name], version) {
			continue
		}
		set[name] = append(set[name], version)
	}
	return set, true
}
