package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/internal/oci"
	"github.com/varroaci/varroa-jenkins/internal/updatecenter"
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		root.AddCommand(newImportCmd())
	})
}

func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import --from <src> --to <dest>",
		Short: "Import (copy) a plugin pack between OCI destinations",
		Long: `Copy a plugin pack between OCI-compatible sources and destinations.

Supported schemes for --from and --to:
  oci://<registry>/<repo>[:<tag>]   OCI registry
  dir://<path>                      OCI layout directory
  tar://<path>.tar.gz                gzipped tar archive

uc:// is not supported in this build.`,
		RunE: runImport,
	}

	cmd.Flags().String("from", "", "source (oci://, dir://, tar://) (required)")
	_ = cmd.MarkFlagRequired("from")
	cmd.Flags().String("to", "", "destination (oci://, dir://, tar://) (required)")
	_ = cmd.MarkFlagRequired("to")
	cmd.Flags().String("registry-config", "", "path to Docker config.json for registry auth")
	cmd.Flags().Bool("insecure", false, "use plain HTTP for registry")

	return cmd
}

func runImport(cmd *cobra.Command, args []string) error {
	// Validate -o flag
	if cmd.Flags().Changed("output") {
		o, _ := cmd.Flags().GetString("output")
		if o != "json" {
			return usagef("import only supports -o json; got %q", o)
		}
	}

	fromDest, _ := cmd.Flags().GetString("from")
	toDest, _ := cmd.Flags().GetString("to")
	registryConfig, _ := cmd.Flags().GetString("registry-config")
	insecure, _ := cmd.Flags().GetBool("insecure")

	// Parse both destinations
	srcScheme, srcTarget, err := ParseOCIDest(fromDest)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}

	dstScheme, dstTarget, err := ParseOCIDest(toDest)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}

	// Reject uc:// on --from only
	if srcScheme == "uc" {
		return errUCNotSupported
	}

	// Handle uc:// on --to — the UC import path.
	if dstScheme == "uc" {
		return runImportToUC(cmd, srcScheme, srcTarget, dstTarget, registryConfig, insecure)
	}

	// Open source store
	srcStore, srcCleanup, err := openOCISrc(srcScheme, srcTarget, registryConfig, insecure)
	if err != nil {
		return err
	}
	defer func() { _ = srcCleanup() }()

	// Open destination store
	dstStore, dstCleanup, err := openOCIDest(dstScheme, dstTarget, registryConfig, insecure)
	if err != nil {
		return err
	}
	defer func() { _ = dstCleanup() }()

	// Copy
	if err := oci.Copy(cmd.Context(), srcStore, srcTarget, dstStore, dstTarget); err != nil {
		return fmt.Errorf("copy: %w", err)
	}

	// Finalize destination (for tar://)
	if err := dstCleanup(); err != nil {
		return fmt.Errorf("finalize destination: %w", err)
	}

	// Resolve digest from destination
	desc, err := dstStore.Resolve(cmd.Context(), dstTarget)
	if err != nil {
		return fmt.Errorf("resolve manifest: %w", err)
	}

	outputJSON, _ := cmd.Flags().GetString("output")
	if outputJSON == "json" {
		out := map[string]any{
			"digest": desc.Digest,
			"ref":    dstTarget,
		}
		return printJSON(os.Stdout, out)
	}

	fmt.Printf("Imported %s → %s with digest %s\n", fromDest, toDest, desc.Digest)
	return nil
}

// runImportToUC handles --to uc://<host>[:<port>].
// It checks VARROACTL_UC_TOKEN, builds a tar.gz from the source, and POSTs it.
func runImportToUC(cmd *cobra.Command, srcScheme, srcTarget, dstTarget, registryConfig string, insecure bool) error {
	// 1. Check env var BEFORE touching --from or building payload.
	token := os.Getenv("VARROACTL_UC_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "error: VARROACTL_UC_TOKEN is not set")
		fmt.Fprintln(os.Stderr, "  The uc:// destination requires a valid import token.")
		fmt.Fprintln(os.Stderr, "  Set VARROACTL_UC_TOKEN to the token stored in the")
		fmt.Fprintln(os.Stderr, "  varroa-updatecenter-import-token Secret (key: token).")
		return fmt.Errorf("VARROACTL_UC_TOKEN not set")
	}

	// 2. Open source store.
	srcStore, srcCleanup, err := openOCISrc(srcScheme, srcTarget, registryConfig, insecure)
	if err != nil {
		return err
	}
	defer func() { _ = srcCleanup() }()

	// 3. Create temp LayoutStore, copy source into it.
	tmpDir, err := os.MkdirTemp("", "varroactl-uc-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpStore, err := oci.NewLayoutStore(tmpDir)
	if err != nil {
		return fmt.Errorf("open temp layout store: %w", err)
	}

	if err := oci.Copy(cmd.Context(), srcStore, srcTarget, tmpStore, srcTarget); err != nil {
		return fmt.Errorf("copy to temp layout: %w", err)
	}

	// 4. Archive temp dir to tar.gz in memory.
	var tarGzBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarGzBuf)
	tw := tar.NewWriter(gzw)

	if err := filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(tmpDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, f); err != nil {
				_ = f.Close()
				return err
			}
			_ = f.Close()
		}
		return nil
	}); err != nil {
		return fmt.Errorf("walk temp dir for tar: %w", err)
	}
	_ = tw.Close()
	_ = gzw.Close()

	// 5. Parse host:port from dstTarget. Default port 80.
	host, portStr, err := net.SplitHostPort(dstTarget)
	if err != nil {
		// Assume dstTarget is just a hostname without port.
		host = dstTarget
		portStr = "80"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("invalid port in uc://%s: %w", dstTarget, err)
	}
	targetURL := fmt.Sprintf("http://%s:%d", host, port)

	// 6. POST import.
	if err := updatecenter.PostImport(cmd.Context(), targetURL, token, &tarGzBuf); err != nil {
		if errors.Is(err, updatecenter.ErrInvalidToken) {
			fmt.Fprintln(os.Stderr, "error: invalid or expired import token")
			fmt.Fprintln(os.Stderr, "  The uc:// server rejected the VARROACTL_UC_TOKEN.")
			fmt.Fprintln(os.Stderr, "  Verify the token matches the varroa-updatecenter-import-token Secret.")
			return fmt.Errorf("import token rejected")
		}
		return fmt.Errorf("post import: %w", err)
	}

	// 7. Success output.
	outputJSON, _ := cmd.Flags().GetString("output")
	if outputJSON == "json" {
		out := map[string]any{
			"ref": "uc://" + dstTarget,
		}
		return printJSON(os.Stdout, out)
	}

	fmt.Printf("Imported %s → uc://%s\n", cmd.Flag("from").Value.String(), dstTarget)
	return nil
}
