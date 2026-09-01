package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/pluginresolve"
)

const defaultDownloadURLBase = "https://updates.jenkins.io"

type resolveOptions struct {
	HPIPath     string
	PluginsPath string
	DownloadURL string
	Indent      int
	Fetch       pluginresolve.Fetcher
}

func runResolve(ctx context.Context, opts resolveOptions, stdout io.Writer) error {
	rootBytes, err := os.ReadFile(opts.HPIPath) // #nosec G304 -- maintenance tool, path is a flag
	if err != nil {
		return fmt.Errorf("read root HPI: %w", err)
	}
	resolved, err := readPluginSet(opts.PluginsPath)
	if err != nil {
		return err
	}

	entries, err := pluginresolve.ResolveClosure(ctx, rootBytes, resolved, opts.Fetch)
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

// writeBootstrapYAML emits the closure as a `bootstrap:` block indented to sit
// beside a lock set's `core:` and `plugins:` keys.
func writeBootstrapYAML(w io.Writer, entries []pluginresolve.BootstrapEntry, indent int) error {
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
