package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
)

const defaultDownloadURLBase = "https://updates.jenkins.io"

// fetcher retrieves a plugin's .hpi bytes at a pinned version.
type fetcher func(name, version string) ([]byte, error)

// bootstrapEntry mirrors pluginlock.BootstrapEntry. It is declared here rather
// than imported so --resolve stays a pure text-in/text-out tool with no
// dependency on the embedded lock it is generating.
type bootstrapEntry struct {
	ArtifactID string
	Version    string
	Mins       []string
}

type resolveOptions struct {
	HPIPath     string
	PluginsPath string
	DownloadURL string
	Indent      int
	Fetch       fetcher
}

func httpFetcher(base string) fetcher {
	base = strings.TrimRight(base, "/")
	return func(name, version string) ([]byte, error) {
		url := fmt.Sprintf("%s/download/plugins/%s/%s/%s.hpi", base, name, version, name)
		resp, err := http.Get(url) // #nosec G107 -- base URL is an operator-supplied flag on a maintenance tool
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
}

func runResolve(opts resolveOptions, stdout io.Writer) error {
	rootBytes, err := os.ReadFile(opts.HPIPath) // #nosec G304 -- maintenance tool, path is a flag
	if err != nil {
		return fmt.Errorf("read root HPI: %w", err)
	}
	resolved, err := readPluginSet(opts.PluginsPath)
	if err != nil {
		return err
	}

	entries, err := resolveClosure(rootBytes, resolved, opts.Fetch)
	if err != nil {
		return err
	}
	return writeBootstrapYAML(stdout, entries, opts.Indent)
}

// readPluginSet parses the generator's resolved plugin list — one
// `name:version` per line, as jenkins-plugin-cli --list emits.
func readPluginSet(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- maintenance tool, path is a flag
	if err != nil {
		return nil, fmt.Errorf("read plugin set: %w", err)
	}
	defer func() { _ = f.Close() }()

	set := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, version, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name, version = strings.TrimSpace(name), strings.TrimSpace(version)
		if name != "" && version != "" {
			set[name] = version
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read plugin set: %w", err)
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("plugin set %q is empty", path)
	}
	return set, nil
}

// normalizeRootVersion truncates at the first space. A snapshot build stamps
// `1.0-SNAPSHOT (private-<timestamp>-<user>)` into Plugin-Version, and that
// suffix must never reach a committed lock.
func normalizeRootVersion(v string) string {
	if i := strings.IndexByte(v, ' '); i >= 0 {
		return v[:i]
	}
	return v
}

// resolveClosure walks varroa-mite-auth's MANDATORY dependency closure.
//
// Optional dependencies are neither required nor traversed: Jenkins itself
// tolerates their absence, so requiring one would assert something Jenkins does
// not. Every member must be present in the resolved set; the ROOT is exempt,
// because it is baked into the image and is not, and never will be, a lock
// member. Presence is the whole assertion — no version comparison happens here.
//
// The visited set is keyed by name, so a diamond fetches each HPI at most once.
func resolveClosure(rootHPI []byte, resolved map[string]string, fetch fetcher) ([]bootstrapEntry, error) {
	rootMF, err := hpi.ParseHPIBytes(rootHPI)
	if err != nil {
		return nil, fmt.Errorf("parse root HPI: %w", err)
	}

	root := bootstrapEntry{
		ArtifactID: rootMF.ShortName,
		Version:    normalizeRootVersion(rootMF.Version),
	}

	// parent tracks how each member was reached, so a failure can print the
	// full chain from the root and the operator sees WHY a seemingly unrelated
	// plugin is required.
	parent := map[string]string{}
	mins := map[string]map[string]struct{}{}
	order := []string{}
	visited := map[string]bool{rootMF.ShortName: true}

	type queued struct {
		name string
		deps []hpi.Dependency
	}
	queue := []queued{{name: rootMF.ShortName, deps: rootMF.Dependencies}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, d := range cur.deps {
			if d.Optional {
				continue
			}
			if _, ok := parent[d.Name]; !ok {
				parent[d.Name] = cur.name
			}
			pin, present := resolved[d.Name]
			if !present {
				return nil, fmt.Errorf(
					"mandatory dependency %q is missing from the resolved plugin set; required via %s",
					d.Name, renderChain(parent, rootMF.ShortName, d.Name))
			}
			if mins[d.Name] == nil {
				mins[d.Name] = map[string]struct{}{}
			}
			mins[d.Name][d.Min] = struct{}{}

			if visited[d.Name] {
				continue
			}
			visited[d.Name] = true
			order = append(order, d.Name)

			b, err := fetch(d.Name, pin)
			if err != nil {
				return nil, fmt.Errorf("fetch %s@%s (required via %s): %w",
					d.Name, pin, renderChain(parent, rootMF.ShortName, d.Name), err)
			}
			mf, err := hpi.ParseHPIBytes(b)
			if err != nil {
				return nil, fmt.Errorf("parse %s@%s: %w", d.Name, pin, err)
			}
			queue = append(queue, queued{name: d.Name, deps: mf.Dependencies})
		}
	}

	out := make([]bootstrapEntry, 0, len(order)+1)
	out = append(out, root)
	for _, name := range order {
		declared := make([]string, 0, len(mins[name]))
		for m := range mins[name] {
			declared = append(declared, m)
		}
		// De-duplicated by exact string equality and sorted lexicographically
		// for byte-stability across runs. NOT reduced to a greatest minimum:
		// picking one would require comparing versions.
		sort.Strings(declared)
		out = append(out, bootstrapEntry{
			ArtifactID: name,
			Version:    resolved[name],
			Mins:       declared,
		})
	}
	return out, nil
}

// renderChain renders "root → … → target" using the recorded parent links.
func renderChain(parent map[string]string, root, target string) string {
	chain := []string{target}
	seen := map[string]bool{target: true}
	for cur := target; cur != root; {
		p, ok := parent[cur]
		if !ok || seen[p] {
			break
		}
		chain = append(chain, p)
		seen[p] = true
		cur = p
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return strings.Join(chain, " → ")
}

// writeBootstrapYAML emits the closure as a `bootstrap:` block indented to sit
// beside a lock set's `core:` and `plugins:` keys.
func writeBootstrapYAML(w io.Writer, entries []bootstrapEntry, indent int) error {
	pad := strings.Repeat(" ", indent)
	item := pad + "  "
	field := pad + "    "
	if _, err := fmt.Fprintf(w, "%sbootstrap:\n", pad); err != nil {
		return err
	}
	for _, e := range entries {
		// Versions are quoted: a pin like `2.1` unquoted is a YAML float, and
		// decoding one into a string field fails outright.
		if _, err := fmt.Fprintf(w, "%s- artifactId: %s\n%sversion: %q\n", item, e.ArtifactID, field, e.Version); err != nil {
			return err
		}
		if len(e.Mins) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "%smins:\n", field); err != nil {
			return err
		}
		for _, m := range e.Mins {
			if _, err := fmt.Fprintf(w, "%s  - %q\n", field, m); err != nil {
				return err
			}
		}
	}
	return nil
}
