package main

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/internal/hpi"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		exportCmd := findCommand(root, "export")
		if exportCmd == nil {
			exportCmd = newExportCmd()
			root.AddCommand(exportCmd)
		}
		exportCmd.AddCommand(newExportAddonCmd())
	})
}

func newExportAddonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin-addon --hpi <path> --to <dest> [flags]",
		Short: "Build a single-plugin addon pack from a local .hpi",
		Long: `plugin-addon packs one local .hpi file into an OCI addon pack.

The plugin's name and version come from the archive's META-INF/MANIFEST.MF and
can never be overridden by a flag, so a pack cannot be mislabeled relative to
the bytes it holds.

The --to destination uses the following schemes:
  oci://<registry>/<repo>[:<tag>]   push to an OCI registry
  dir://<path>                      write to an OCI layout directory
  tar://<path>.tar.gz               write to a gzipped tar archive

A version-bearing tag such as plugin-addon:varroa-mcp-tools-1.0.0 is a naming
convention. Nothing here makes a tag immutable: this command neither reads an
existing tag nor refuses to overwrite one.`,
		RunE: runExportAddon,
	}

	cmd.Flags().String("hpi", "", "path to the .hpi to pack (required)")
	_ = cmd.MarkFlagRequired("hpi")
	cmd.Flags().String("to", "", "destination (oci://, dir://, tar://) (required)")
	_ = cmd.MarkFlagRequired("to")
	cmd.Flags().StringArray("tag", nil, "free-form tag recorded on the plugin layer (repeatable)")
	cmd.Flags().String("description", "", "description recorded on the plugin layer")
	cmd.Flags().String("registry-config", "", "path to Docker config.json for registry auth")
	cmd.Flags().Bool("insecure", false, "use plain HTTP for registry")
	cmd.Flags().Bool("dry-run", false, "print the resolved pack config and annotations without pushing")

	return cmd
}

func runExportAddon(cmd *cobra.Command, _ []string) error {
	if cmd.Flags().Changed("output") {
		o, _ := cmd.Flags().GetString("output")
		if o != "json" {
			return usagef("export plugin-addon only supports -o json; got %q", o)
		}
	}

	hpiPath, _ := cmd.Flags().GetString("hpi")
	toDest, _ := cmd.Flags().GetString("to")
	tags, _ := cmd.Flags().GetStringArray("tag")
	description, _ := cmd.Flags().GetString("description")
	registryConfig, _ := cmd.Flags().GetString("registry-config")
	insecure, _ := cmd.Flags().GetBool("insecure")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	scheme, target, err := ParseOCIDest(toDest)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}
	// An addon pack is built locally; uc:// is an import endpoint, not a build
	// destination. Reject it before reading anything.
	if scheme == "uc" {
		return usagef("uc:// is not a valid destination for export plugin-addon; build to oci://, dir://, or tar:// and import from there")
	}

	// Read once: the same bytes are both parsed and digested, so the recorded
	// sha256 cannot describe a different file than the recorded identity.
	data, err := os.ReadFile(hpiPath) // #nosec G304 -- operator-supplied path on an operator-run CLI
	if err != nil {
		return fmt.Errorf("read %s: %w", hpiPath, err)
	}
	mf, err := hpi.ParseHPIBytes(data)
	if err != nil {
		return fmt.Errorf("%s: %w", hpiPath, err)
	}

	digest, _, err := oci.Sha256Digest(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("digest %s: %w", hpiPath, err)
	}

	plugin := oci.ResolvedPlugin{
		Name:         mf.ShortName,
		Version:      mf.Version,
		SHA256:       digest,
		UpstreamURL:  "", // a local artifact has no upstream
		DisplayName:  mf.LongName,
		Description:  description,
		Tags:         tags,
		RequiredCore: mf.RequiredCore,
		Dependencies: mf.Dependencies,
		Content:      bytes.NewReader(data),
	}
	plugins := []oci.ResolvedPlugin{plugin}

	cfg := oci.PackConfig{
		Kind:           oci.PackKindAddon,
		JenkinsVersion: mf.RequiredCore,
		Profile:        "",
		LockHash:       oci.LockHash(plugins),
		PluginCount:    1,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	annotations, err := oci.PluginLayerAnnotations(plugin)
	if err != nil {
		return err
	}

	if dryRun {
		return printJSON(os.Stdout, map[string]any{
			"dryRun":      true,
			"name":        mf.ShortName,
			"version":     mf.Version,
			"config":      cfg,
			"annotations": annotations,
			"ref":         toDest,
		})
	}

	store, finalize, err := openOCIDest(scheme, target, registryConfig, insecure)
	if err != nil {
		return err
	}

	if err := oci.BuildPluginPack(cmd.Context(), store, target, cfg, plugins); err != nil {
		return fmt.Errorf("build addon pack: %w", err)
	}

	// Resolve before finalize: tar:// removes the staging layout in finalize.
	desc, err := store.Resolve(cmd.Context(), target)
	if err != nil {
		return fmt.Errorf("resolve pushed addon pack: %w", err)
	}

	if err := finalize(); err != nil {
		return err
	}

	if o, _ := cmd.Flags().GetString("output"); o == "json" {
		return printJSON(os.Stdout, map[string]any{
			"digest":  desc.Digest,
			"name":    mf.ShortName,
			"version": mf.Version,
			"ref":     toDest,
		})
	}

	fmt.Printf("Exported addon %s@%s with digest %s\n", mf.ShortName, mf.Version, desc.Digest)
	return nil
}
