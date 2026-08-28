package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/internal/bundle"
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
		Long: `Export clones a bundle repository, tar+gzips the bundle directory,
and pushes the result to the destination OCI store.

The --to destination uses the following schemes:
  oci://<registry>/<repo>[:<tag>]   push to an OCI registry
  dir://<path>                      write to an OCI layout directory
  tar://<path>.tar.gz                write to a gzipped tar archive`,
		RunE: runExportBundle,
	}

	cmd.Flags().String("repo", "", "repository URL (required)")
	_ = cmd.MarkFlagRequired("repo")
	cmd.Flags().String("path", "", "bundle path within the repo (default \".\")")
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

	resolver := bundle.NewResolver(tmpDir)

	if _, err := resolver.Materialize(cmd.Context(), repo, bundlePath, revision, filepath.Join(tmpDir, "bundle"), nil); err != nil {
		return fmt.Errorf("materialize bundle: %w", err)
	}

	// Build a ref from the repo name.
	ref := strings.TrimPrefix(repo, "https://")
	ref = strings.TrimPrefix(ref, "ssh://")
	ref = strings.TrimPrefix(ref, "git@")
	ref = strings.ReplaceAll(ref, "/", "-")
	ref = strings.ReplaceAll(ref, ":", "-")
	ref = strings.TrimSuffix(ref, ".git")
	if ref == "" {
		ref = "bundle"
	}

	// Push as OCI artifact.
	if err := pushBundleAsOCIArtifact(cmd.Context(), store, ref, filepath.Join(tmpDir, "bundle"), "application/vnd.varroa.bundle.v1.tar+gzip", "application/vnd.varroa.bundle.v1"); err != nil {
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

	fmt.Printf("Exported bundle %s with digest %s\n", repo, desc.Digest)
	return nil
}
