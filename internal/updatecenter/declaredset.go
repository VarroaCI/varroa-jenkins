package updatecenter

import (
	"os"
	"slices"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/pluginver"
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

// Highest returns the highest declared version for name by pluginver ordering.
// The tier is evaluated against the highest declared version, so a lack of
// deduplication upstream needs no notion of a unique pin here.
func (d DeclaredSet) Highest(name string) (string, bool) {
	versions := d[name]
	if len(versions) == 0 {
		return "", false
	}
	best := versions[0]
	for _, v := range versions[1:] {
		if pluginver.Compare(v, best) > 0 {
			best = v
		}
	}
	return best, true
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
