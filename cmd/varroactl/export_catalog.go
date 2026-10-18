package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/oci"
)

func init() {
	rootRegistrars = append(rootRegistrars, func(root *cobra.Command) {
		exportCmd := findSubCommand(root, "export")
		if exportCmd == nil {
			exportCmd = newExportCmd()
			root.AddCommand(exportCmd)
		}
		exportCmd.AddCommand(newExportCatalogCmd())
	})
}

func newExportCatalogCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog --namespace <ns> --name <catalogsource> --to <dest>",
		Short: "Export a CatalogSource to an OCI destination",
		Long: `Export a CatalogSource as a catalog artifact.

The command fetches the source tree from git or from an OCI artifact. It reads
the catalog index from catalog.yaml, or from the directory convention when
catalog.yaml is absent. It then pushes the whole source tree as one catalog
layer. The layer root is the source root, so an imported CatalogSource keeps
the same spec.path.

The manifest reference is the --to target. An oci:// target without a tag
takes the CatalogSource name as its tag.

The --to destination uses the following schemes:
  oci://<registry>/<repo>[:<tag>]   push to an OCI registry
  dir://<path>                      write to an OCI layout directory
  tar://<path>.tar.gz                write to a gzipped tar archive`,
		RunE: runExportCatalog,
	}

	cmd.Flags().String("namespace", "", "namespace of the CatalogSource (required)")
	_ = cmd.MarkFlagRequired("namespace")
	cmd.Flags().String("name", "", "name of the CatalogSource (required)")
	_ = cmd.MarkFlagRequired("name")
	cmd.Flags().String("to", "", "destination (oci://, dir://, tar://) (required)")
	_ = cmd.MarkFlagRequired("to")
	cmd.Flags().String("registry-config", "", "path to Docker config.json for registry auth")
	cmd.Flags().Bool("insecure", false, "use plain HTTP for registry")
	addClusterFlag(cmd)

	return cmd
}

// removeSymlinks deletes every symbolic link under root. A link can dangle or
// point at a directory, and the layer writer cannot pack either.
func removeSymlinks(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return os.Remove(path)
		}
		return nil
	})
}

// newCatalogCloner returns the git cloner that export catalog uses. Tests
// replace it to permit local repository fixtures.
var newCatalogCloner = bundle.NewGitCloner

func runExportCatalog(cmd *cobra.Command, args []string) error {
	ns, _ := cmd.Flags().GetString("namespace")
	name, _ := cmd.Flags().GetString("name")
	toDest, _ := cmd.Flags().GetString("to")
	registryConfig, _ := cmd.Flags().GetString("registry-config")
	insecure, _ := cmd.Flags().GetBool("insecure")

	// Parse the OCI dest scheme.
	scheme, target, err := ParseOCIDest(toDest)
	if err != nil {
		if _, ok := err.(*ErrUnrecognizedScheme); ok {
			return usagef("%v", err)
		}
		return err
	}

	if scheme == "uc" {
		return rejectUCSnapshotDest("catalog")
	}

	cluster := resolveCrdCluster(cmd)

	// Fetch CatalogSource from BFF.
	resp, err := rawRequest(cmd, http.MethodGet, "/clusters/"+cluster+"/catalogsources/"+ns+"/"+name, nil)
	if err != nil {
		return fmt.Errorf("fetch CatalogSource: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fetch CatalogSource: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read CatalogSource response: %w", err)
	}

	var src v1alpha1.CatalogSource
	if err := json.Unmarshal(body, &src); err != nil {
		return fmt.Errorf("decode CatalogSource: %w", err)
	}

	spec := src.Spec
	if spec.RepoURL == "" && spec.OCIRef == "" {
		return fmt.Errorf("CatalogSource %s/%s has neither repoURL nor ociRef", ns, name)
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

	// Fetch the source tree into a temp dir, with the same fetch as the catalog controller.
	tmpDir, err := os.MkdirTemp("", "varroactl-catalog-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	cloneDir := filepath.Join(tmpDir, "catalog")
	if err := os.MkdirAll(cloneDir, 0755); err != nil {
		return fmt.Errorf("create catalog dir: %w", err)
	}

	fetch := bundle.CatalogFetch{
		RepoURL:  spec.RepoURL,
		Revision: spec.Revision,
		OCIRef:   spec.OCIRef,
		Cloner:   newCatalogCloner(),
	}
	if spec.SecretRef != "" {
		secData, err := getLocalSecret(cmd, spec.SecretRef, ns)
		if err != nil {
			return fmt.Errorf("read auth secret %s: %w", spec.SecretRef, err)
		}
		if spec.OCIRef != "" {
			fetch.OCIAuth, err = bundle.OCIAuthFromSecret(secData)
			if err != nil {
				return fmt.Errorf("bad OCI auth secret %s: %w", spec.SecretRef, err)
			}
		} else {
			fetch.GitAuth, err = bundle.GitAuthFromSecret(secData, "")
			if err != nil {
				return fmt.Errorf("bad git auth secret %s: %w", spec.SecretRef, err)
			}
		}
	}

	rev, err := bundle.FetchCatalog(cmd.Context(), fetch, cloneDir)
	if err != nil {
		return fmt.Errorf("fetch catalog: %w", err)
	}
	// The controller and import skip symlinks, so the exported tree does too.
	if err := removeSymlinks(cloneDir); err != nil {
		return fmt.Errorf("prepare catalog tree: %w", err)
	}

	root, err := bundle.CatalogRoot(cloneDir, spec.Path)
	if err != nil {
		return err
	}
	index, err := bundle.ParseCatalogIndex(root)
	if err != nil {
		return fmt.Errorf("parse catalog index: %w", err)
	}
	if len(index.Items) == 0 {
		fmt.Fprintf(os.Stderr, "warning: CatalogSource %s/%s has no catalog items\n", ns, name)
	}

	// The manifest reference is the --to target. An untagged oci:// target
	// takes the CatalogSource name as its tag.
	ref := target
	if scheme == "oci" && !hasExplicitTag(target) {
		ref = target + ":" + name
	}

	// The layer holds the whole source tree. spec.path applies on import.
	if err := pushBundleAsOCIArtifact(cmd.Context(), store, ref, cloneDir, bundle.CatalogLayerMediaType, bundle.CatalogArtifactType, nil); err != nil {
		return fmt.Errorf("push catalog artifact: %w", err)
	}

	fmt.Printf("Exported CatalogSource %s/%s (%d items, revision %s) as OCI artifact at %s\n", ns, name, len(index.Items), rev, toDest)
	return nil
}

// pushBundleAsOCIArtifact creates a tar.gz of srcDir, pushes it as a blob,
// and pushes a manifest referencing that layer under the given ref. The
// annotations map becomes the manifest annotations; it may be nil.
func pushBundleAsOCIArtifact(ctx context.Context, store oci.BlobStore, ref, srcDir, layerMediaType, artifactType string, annotations map[string]string) error {
	// Create tar.gz of the bundle directory.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return fmt.Errorf("rel path: %w", err)
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("tar header for %q: %w", path, err)
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write tar header %q: %w", rel, err)
		}
		if info.IsDir() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %q: %w", path, err)
		}
		defer func() { _ = src.Close() }()
		if _, err := io.Copy(tw, src); err != nil {
			return fmt.Errorf("copy %q: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("close gzip writer: %w", err)
	}

	layerData := buf.Bytes()

	// Push the layer blob.
	layerDigest, layerSize, err := store.PushBlob(ctx, layerMediaType, bytes.NewReader(layerData))
	if err != nil {
		return fmt.Errorf("push layer blob: %w", err)
	}

	// Push a config blob (empty JSON).
	configDigest, _, err := store.PushBlob(ctx, "application/vnd.varroa.bundle.config.v1+json", strings.NewReader("{}"))
	if err != nil {
		return fmt.Errorf("push config blob: %w", err)
	}

	// Push the manifest.
	return store.Push(ctx, ref, oci.Manifest{
		ArtifactType: artifactType,
		Annotations:  annotations,
		Config: oci.Descriptor{
			MediaType: "application/vnd.varroa.bundle.config.v1+json",
			Digest:    configDigest,
		},
		Layers: []oci.Descriptor{
			{
				MediaType: layerMediaType,
				Digest:    layerDigest,
				Size:      layerSize,
			},
		},
	})
}

// getLocalSecret reads a Secret from the local k8s API (via the BFF proxy path).
func getLocalSecret(cmd *cobra.Command, name, namespace string) (map[string][]byte, error) {
	cluster := resolveCrdCluster(cmd)
	resp, err := rawRequest(cmd, http.MethodGet, "/clusters/"+cluster+"/namespaces/"+namespace+"/secrets/"+name, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch secret %s/%s: %w", namespace, name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch secret %s/%s: HTTP %d: %s", namespace, name, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read secret response: %w", err)
	}

	var secretData map[string]string
	if err := json.Unmarshal(body, &secretData); err != nil {
		return nil, fmt.Errorf("decode secret: %w", err)
	}

	// Convert string map to byte map.
	result := make(map[string][]byte, len(secretData))
	for k, v := range secretData {
		result[k] = []byte(v)
	}
	return result, nil
}

// findSubCommand locates a subcommand by name on a cobra command.
func findSubCommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}
