// Command bootstrapdeps asserts that the plugins varroa-mite-auth mandatorily
// depends on are present in a resolved Jenkins plugin set, and records the
// closure it walked.
//
// varroa-mite-auth is baked into the operator image and copied into every
// Jenkins pod, so it is never itself a member of the resolved lock. Its
// mandatory dependency closure, however, must be — a lock regeneration that
// drops `mailer` silently breaks authentication on every controller, and the
// first symptom is a Jenkins nobody can log into. This tool moves that failure
// from Jenkins boot on every controller to one script run at review time.
//
// It is repo-internal tooling, following the cmd/protogen precedent: it is not
// shipped in the runtime image and is deliberately not a varroactl subcommand,
// because lock generation is a repo-maintenance concern.
//
// Two modes:
//
//	--resolve  (network)  walk the closure from a built HPI and emit it as YAML,
//	                      for hack/gen-plugin-lock.sh to write into lock.yaml
//	--check    (offline)  re-verify a committed bootstrap block against the
//	                      committed plugin set, for pr.yaml
//
// Neither mode compares versions. Presence is the assertion; declared minimums
// are recorded verbatim for a later consumer that owns a comparator.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/varroaci/varroa-jenkins/internal/pluginresolve"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrapdeps: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("bootstrapdeps", flag.ContinueOnError)
	var (
		doResolve   = fs.Bool("resolve", false, "walk the closure from a built HPI and emit it as YAML (needs network)")
		doCheck     = fs.Bool("check", false, "verify a committed bootstrap block against the committed plugin set (offline)")
		hpiPath     = fs.String("hpi", "", "--resolve: path to the built varroa-mite-auth HPI")
		pluginsPath = fs.String("plugins", "", "--resolve: path to the resolved plugin set (name:version per line)")
		downloadURL = fs.String("download-url-base", defaultDownloadURLBase, "--resolve: base URL for plugin downloads")
		indent      = fs.Int("indent", 4, "--resolve: leading spaces for the emitted `bootstrap:` key")
		lockPath    = fs.String("lock", defaultLockPath, "--check: path to lock.yaml")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case *doResolve && *doCheck:
		return fmt.Errorf("--resolve and --check are mutually exclusive")
	case *doResolve:
		if *hpiPath == "" || *pluginsPath == "" {
			return fmt.Errorf("--resolve requires --hpi and --plugins")
		}
		return runResolve(context.Background(), resolveOptions{
			HPIPath:     *hpiPath,
			PluginsPath: *pluginsPath,
			DownloadURL: *downloadURL,
			Indent:      *indent,
			Fetch:       pluginresolve.HTTPFetcher(*downloadURL),
		}, stdout)
	case *doCheck:
		return runCheck(*lockPath, stdout)
	default:
		fs.Usage()
		return fmt.Errorf("one of --resolve or --check is required")
	}
}
