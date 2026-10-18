package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/varroaci/varroa-jenkins/internal/bundle"
)

// newExportBundleResolver builds the resolver that materializes the source
// bundle. Tests replace it to reach a local fixture repository.
var newExportBundleResolver = func(workDir string) *bundle.Resolver {
	return bundle.NewResolver(noop.NewMeterProvider(), workDir)
}

var (
	repoSchemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
	repoUserRE   = regexp.MustCompile(`^[^@/]+@`)
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		exportCmd := findSubCommand(root, "export")
		if exportCmd == nil {
			exportCmd = newExportCmd()
			root.AddCommand(exportCmd)
		}
		exportCmd.AddCommand(newExportBundleCmd())
	})
}

func newExportBundleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bundle --repo <url> --path <path> --revision <revision> --to <dest>",
		Short: "Export a git bundle as an OCI artifact",
		Long: `Export clones a bundle repository, tar+gzips the --path directory,
and pushes the result to the destination OCI store. The layer root is the
bundle directory, so the artifact needs only ociSource.ref to import.

The --to destination uses the following schemes:
  oci://<registry>/<repo>[:<tag>]   push to an OCI registry
  dir://<path>                      write to an OCI layout directory
  tar://<path>.tar.gz                write to a gzipped tar archive

A dir:// or tar:// export is tagged <repo>-<path>. An oci:// destination
with an explicit :<tag> uses that tag. Otherwise it uses <repo>-<path>.
The manifest records the source URL (credentials removed), the commit
SHA, and the bundle path.`,
		RunE: runExportBundle,
	}

	cmd.Flags().String("repo", "", "repository URL (required)")
	_ = cmd.MarkFlagRequired("repo")
	cmd.Flags().String("path", "", "bundle directory within the repo; only this directory is exported (default \".\")")
	cmd.Flags().String("revision", "", "revision (branch, tag, or commit SHA)")
	cmd.Flags().String("to", "", "destination (oci://, dir://, tar://) (required)")
	_ = cmd.MarkFlagRequired("to")
	cmd.Flags().String("registry-config", "", "path to Docker config.json for registry auth")
	cmd.Flags().Bool("insecure", false, "use plain HTTP for registry")

	return cmd
}

func runExportBundle(cmd *cobra.Command, args []string) error {
	repo, _ := cmd.Flags().GetString("repo")
	bundlePath, _ := cmd.Flags().GetString("path")
	revision, _ := cmd.Flags().GetString("revision")
	toDest, _ := cmd.Flags().GetString("to")
	registryConfig, _ := cmd.Flags().GetString("registry-config")
	insecure, _ := cmd.Flags().GetBool("insecure")

	if bundlePath == "" {
		bundlePath = "."
	}

	// Validate -o flag
	if cmd.Flags().Changed("output") {
		o, _ := cmd.Flags().GetString("output")
		if o != "json" {
			return usagef("export only supports -o json; got %q", o)
		}
	}

	// Parse the OCI dest scheme.
	scheme, target, err := ParseOCIDest(toDest)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}

	if scheme == "uc" {
		return rejectUCSnapshotDest("bundle")
	}

	// Open the destination store.
	store, finalize, err := openOCIDest(scheme, target, registryConfig, insecure)
	if err != nil {
		return err
	}
	defer func() {
		if ferr := finalize(); ferr != nil && err == nil {
			err = fmt.Errorf("finalize destination: %w", ferr)
		}
	}()

	// Clone and materialize the bundle into a temp dir.
	tmpDir, err := os.MkdirTemp("", "varroactl-bundle-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	resolver := newExportBundleResolver(tmpDir)

	mat, err := resolver.Materialize(cmd.Context(), repo, bundlePath, revision, filepath.Join(tmpDir, "bundle"), nil)
	if err != nil {
		return fmt.Errorf("materialize bundle: %w", err)
	}

	ref := exportBundleRef(repo, bundlePath)
	if scheme == "oci" && hasExplicitTag(target) {
		ref = target
	}

	cleanPath := path.Clean(filepath.ToSlash(bundlePath))
	annotations := map[string]string{
		"org.opencontainers.image.source": stripRepoUserinfo(repo),
		"dev.varroa.bundle.path":          cleanPath,
	}
	if mat.CommitSHA != "" {
		annotations["org.opencontainers.image.revision"] = mat.CommitSHA
	}

	// Push as OCI artifact. The layer root is the resolved bundle directory.
	if err := pushBundleAsOCIArtifact(cmd.Context(), store, ref, mat.Dir, "application/vnd.varroa.bundle.v1.tar+gzip", "application/vnd.varroa.bundle.v1", annotations); err != nil {
		return fmt.Errorf("push bundle artifact: %w", err)
	}

	// Print digest.
	desc, err := store.Resolve(cmd.Context(), ref)
	if err != nil {
		return fmt.Errorf("resolve manifest: %w", err)
	}

	outputJSON, _ := cmd.Flags().GetString("output")
	if outputJSON == "json" {
		out := map[string]any{
			"digest": desc.Digest,
			"ref":    ref,
		}
		return printJSON(os.Stdout, out)
	}

	fmt.Printf("Exported bundle %s with digest %s\n", stripRepoUserinfo(repo), desc.Digest)
	return nil
}

// exportBundleRef derives the artifact ref for a bundle export. It is the
// repository slug, followed by the bundle path slug when the path is not the
// repository root. Credentials and the URL scheme are removed.
func exportBundleRef(repo, bundlePath string) string {
	slug := stripRepoUserinfo(repo)
	slug = repoSchemeRE.ReplaceAllString(slug, "")
	slug = repoUserRE.ReplaceAllString(slug, "")
	slug = strings.NewReplacer("/", "-", ":", "-").Replace(slug)
	slug = strings.Trim(strings.TrimSuffix(slug, ".git"), "-")
	if slug == "" {
		slug = "bundle"
	}

	p := strings.Trim(path.Clean(filepath.ToSlash(bundlePath)), "/")
	if p == "." || p == "" {
		return slug
	}
	return slug + "-" + strings.ReplaceAll(p, "/", "-")
}

// stripRepoUserinfo removes the user and password, and any query or fragment,
// from a URL repository. It works on the text, so a password that url.Parse
// rejects is still removed. An scp-style address such as user@host:path has
// no scheme and is returned unchanged.
func stripRepoUserinfo(repo string) string {
	loc := repoSchemeRE.FindStringIndex(repo)
	if loc == nil {
		return repo
	}
	// The userinfo ends at the last "@". A password may hold "/", "?", or "#",
	// so the search does not stop at those. A path that holds "@" loses the
	// text before it; only the derived name changes, not the clone URL.
	rest := repo[loc[1]:]
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	return repo[:loc[1]] + rest
}
