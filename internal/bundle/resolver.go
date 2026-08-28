package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/varroaci/varroa-jenkins/internal/oci"
)

var (
	bundleCloneDuration, _ = otel.Meter("varroa-operator").Float64Histogram("varroa.bundle.clone.duration",
		metric.WithUnit("s"),
	)
)

// Variables holds resolved bundle variables.
type Variables map[string]string

// ResolvedBundle holds the result of bundle resolution (fully resolved).
type ResolvedBundle struct {
	JenkinsYAML string
	PluginsYAML string
	ItemsYAML   string
	RbacYAML    string
	Variables   Variables
}

// MaterializedBundle holds the result of materializing a bundle from git.
// Content retains unresolved ${var} placeholders. Variables are the raw
// values loaded from variable files (NOT including varroa_* family).
type MaterializedBundle struct {
	JenkinsYAML string
	PluginsYAML string
	ItemsYAML   string
	RbacYAML    string
	Variables   Variables // from variable files only, not including varroa_*
}

// Resolver orchestrates git clone → validate → variable resolution.
type Resolver struct {
	cloner           *GitCloner
	cloneCache       *CloneCache
	validator        *Validator
	workDir          string
	oidcIssuer       string
	oidcClientID     string
	oidcClientSecret string
	oidcUserClaim    string
	oidcGroupClaim   string
	Logger           *slog.Logger
}

// UseCloneCache sets an optional clone cache. When set, Materialize uses the
// cache instead of direct clones. Nil (or unset) means cache is disabled.
func (r *Resolver) UseCloneCache(c *CloneCache) { r.cloneCache = c }

// Cloner returns the GitCloner used by this resolver.
func (r *Resolver) Cloner() *GitCloner { return r.cloner }

// NewResolver creates a new Resolver.
func NewResolver(workDir string) *Resolver {
	return &Resolver{
		cloner:    NewGitCloner(),
		validator: NewValidator(),
		workDir:   workDir,
	}
}

// OIDCIssuer returns the configured OIDC issuer, or "".
func (r *Resolver) OIDCIssuer() string { return r.oidcIssuer }

// OIDCClientID returns the configured OIDC client ID, or "".
func (r *Resolver) OIDCClientID() string { return r.oidcClientID }

// OIDCClientSecret returns the configured OIDC client secret, or "".
func (r *Resolver) OIDCClientSecret() string { return r.oidcClientSecret }

// OIDCUserClaim returns the configured OIDC user claim (CSV), or "".
func (r *Resolver) OIDCUserClaim() string { return r.oidcUserClaim }

// OIDCGroupClaim returns the configured OIDC group claim (may be empty if unset).
func (r *Resolver) OIDCGroupClaim() string { return r.oidcGroupClaim }

// SetOIDCClaims configures the OIDC claim names.
func (r *Resolver) SetOIDCClaims(userClaim, groupClaim string) {
	r.oidcUserClaim = userClaim
	r.oidcGroupClaim = groupClaim
}

// LoginURL derives the Varroa login URL from the redirect URL by stripping
// the /callback suffix and appending /login.
func (r *Resolver) LoginURL(redirectURL string) string {
	if redirectURL == "" {
		return ""
	}
	return strings.TrimSuffix(redirectURL, "/callback") + "/login"
}

// SetOIDCConfig sets the OIDC configuration for auto-injected bundle variables.
func (r *Resolver) SetOIDCConfig(issuer, clientID, clientSecret string) {
	r.oidcIssuer = issuer
	r.oidcClientID = clientID
	r.oidcClientSecret = clientSecret
}

// Materialize clones a bundle repo, validates it against the manifest, merges
// content, and loads variables — but does NOT inject varroa_* variables or
// resolve ${var} placeholders. The returned MaterializedBundle retains
// unresolved ${var} references so it can be reused across controllers.
// auth may be nil for public repositories.
func (r *Resolver) Materialize(ctx context.Context, repoURL, path, revision string, cloneDir string, auth *GitAuth) (*MaterializedBundle, error) {
	if repoURL == "" {
		return nil, fmt.Errorf("repoURL is required")
	}
	if path == "" {
		return nil, fmt.Errorf("bundle path is required")
	}

	if r.Logger != nil {
		r.Logger.Debug("materializing bundle", "ref", repoURL, "path", path, "revision", revision)
	}

	// Clean any previous clone before creating the directory.
	if err := os.RemoveAll(cloneDir); err != nil {
		return nil, fmt.Errorf("clean clone dir: %w", err)
	}
	if err := os.MkdirAll(cloneDir, 0755); err != nil {
		return nil, fmt.Errorf("create clone dir: %w", err)
	}

	if r.Logger != nil {
		r.Logger.Info("cloning repo", "url", repoURL)
	}
	if err := func() error {
		start := time.Now()
		_, span := otel.Tracer("varroa-operator").Start(ctx, "bundle.clone",
			trace.WithAttributes(attribute.String("ref", revision)),
		)
		defer span.End()
		var err error
		if r.cloneCache != nil {
			_, _, err = r.cloneCache.Checkout(ctx, repoURL, revision, cloneDir, auth)
		} else {
			err = r.cloner.Clone(repoURL, revision, cloneDir, auth)
		}
		duration := time.Since(start).Seconds()
		result := "success"
		if err != nil {
			result = "error"
		}
		bundleCloneDuration.Record(ctx, duration, metric.WithAttributes(
			attribute.String("result", result),
		))
		return err
	}(); err != nil {
		if r.Logger != nil {
			r.Logger.Error("clone failed", "error", err, "url", repoURL)
		}
		return nil, fmt.Errorf("clone bundle repo: %w", err)
	}

	bundleDir, err := ResolveContainedPath(cloneDir, path)
	if err != nil {
		return nil, fmt.Errorf("bundle path %q: %w", path, err)
	}
	return r.materializeDir(bundleDir)
}

// materializeDir processes a bundle directory: parses the manifest, validates,
// merges JCasC, reads plugin/item/rbac files, and loads variable files. It is
// shared by Materialize (git clone) and MaterializeOCI (OCI pull).
func (r *Resolver) materializeDir(bundleDir string) (*MaterializedBundle, error) {
	manifest, err := ParseManifest(bundleDir)
	if err != nil {
		return nil, fmt.Errorf("invalid bundle manifest: %w", err)
	}

	validation := r.validator.Validate(bundleDir)
	if !validation.Valid {
		return nil, fmt.Errorf("bundle validation failed: %s", strings.Join(validation.Errors, "; "))
	}

	// Merge JCasC files
	mergeStrategy := manifest.JcascMergeStrategy
	if mergeStrategy == "" {
		mergeStrategy = "errorOnConflict"
	}
	jenkinsYAML, err := mergeJcasc(bundleDir, manifest.Jcasc, mergeStrategy)
	if err != nil {
		return nil, fmt.Errorf("merge jcasc: %w", err)
	}

	// Merge optional file groups
	pluginsYAML, err := readFiles(bundleDir, manifest.Plugins)
	if err != nil {
		return nil, fmt.Errorf("read plugins: %w", err)
	}
	itemsYAML, err := readFiles(bundleDir, manifest.Items)
	if err != nil {
		return nil, fmt.Errorf("read items: %w", err)
	}
	rbacYAML, err := readFiles(bundleDir, manifest.Rbac)
	if err != nil {
		return nil, fmt.Errorf("read rbac: %w", err)
	}

	// Load variables from all referenced variable files
	vars := make(Variables)
	for _, p := range manifest.Variables {
		data, err := os.ReadFile(filepath.Join(bundleDir, p))
		if err != nil {
			return nil, fmt.Errorf("read variables file %s: %w", p, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				vars[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}

	return &MaterializedBundle{
		JenkinsYAML: jenkinsYAML,
		PluginsYAML: pluginsYAML,
		ItemsYAML:   itemsYAML,
		RbacYAML:    rbacYAML,
		Variables:   vars,
	}, nil
}

// MaterializeOCI pulls an OCI artifact bundle, extracts its bundle layer into
// cloneDir, then materializes the result identically to Materialize. auth may
// be nil for public artifacts.
func (r *Resolver) MaterializeOCI(ctx context.Context, ref, path, cloneDir string, auth *OCIAuth) (*MaterializedBundle, error) {
	if ref == "" {
		return nil, fmt.Errorf("OCI ref is required")
	}
	// Path is optional per the CRD contract (omitempty): empty means the
	// artifact root. Normalize here so the containment check below (which
	// rejects the empty string via filepath.IsLocal) sees a valid local path.
	if path == "" {
		path = "."
	}

	if r.Logger != nil {
		r.Logger.Debug("materializing OCI bundle", "ref", ref, "path", path)
	}

	// Clean any previous content before creating the directory.
	if err := os.RemoveAll(cloneDir); err != nil {
		return nil, fmt.Errorf("clean clone dir: %w", err)
	}
	if err := os.MkdirAll(cloneDir, 0755); err != nil {
		return nil, fmt.Errorf("create clone dir: %w", err)
	}

	// Build a temp docker config.json for auth (if any).
	var configPath string
	var configDir string
	if auth != nil {
		var err error
		configPath, err = WriteDockerConfigJSON(auth)
		if err != nil {
			return nil, fmt.Errorf("prepare OCI auth: %w", err)
		}
		configDir = filepath.Dir(configPath)
		defer func() { _ = os.RemoveAll(configDir) }()
	}

	// Construct the registry store.
	store, err := oci.NewRegistryStore(ref, oci.RegistryOptions{
		CredentialConfigPath: configPath,
		Insecure:             false,
	})
	if err != nil {
		return nil, fmt.Errorf("create registry store for %q: %w", ref, err)
	}

	if r.Logger != nil {
		r.Logger.Info("pulling OCI artifact", "ref", ref)
	}

	// Pull the manifest.
	manifest, err := store.Pull(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("pull OCI artifact %q: %w", ref, err)
	}

	if len(manifest.Layers) == 0 {
		return nil, fmt.Errorf("OCI artifact %q has no layers", ref)
	}

	// Fetch the first (bundle) layer — expected to be a .tar.gz bundle snapshot.
	layer := manifest.Layers[0]
	rc, err := store.FetchBlob(ctx, layer.Digest)
	if err != nil {
		return nil, fmt.Errorf("fetch bundle layer %q: %w", layer.Digest, err)
	}
	defer func() { _ = rc.Close() }()

	// Untar + gunzip the bundle layer into cloneDir.
	if err := untarGzip(rc, cloneDir); err != nil {
		return nil, fmt.Errorf("extract bundle layer: %w", err)
	}

	bundleDir, err := ResolveContainedPath(cloneDir, path)
	if err != nil {
		return nil, fmt.Errorf("bundle path %q: %w", path, err)
	}
	return r.materializeDir(bundleDir)
}

// untarGzip decompresses a gzipped tar stream into the given directory.
func untarGzip(r io.Reader, targetDir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("create gzip reader: %w", err)
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}

		// Sanitize path: prevent path traversal via ".." entries.
		cleanPath := filepath.Join(targetDir, filepath.Clean(header.Name))
		if !strings.HasPrefix(cleanPath, filepath.Clean(targetDir)+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes target directory", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(cleanPath, 0755); err != nil {
				return fmt.Errorf("create dir %q: %w", cleanPath, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(cleanPath), 0755); err != nil {
				return fmt.Errorf("create parent dir for %q: %w", cleanPath, err)
			}
			f, err := os.Create(cleanPath)
			if err != nil {
				return fmt.Errorf("create file %q: %w", cleanPath, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write file %q: %w", cleanPath, err)
			}
			_ = f.Close()
			if header.Mode != 0 {
				_ = os.Chmod(cleanPath, os.FileMode(header.Mode))
			}
		case tar.TypeSymlink:
			// Symlinks — skip by default for safety; bundle dirs shouldn't need them.
		default:
			// Skip other types (hard links, devices, etc.) silently.
		}
	}
	return nil
}

// Resolve clones a bundle repo, validates it against the manifest, resolves
// variables, and returns the result. It calls Materialize internally and then
// injects varroa_* variables and resolves all ${var} placeholders.
func (r *Resolver) Resolve(repoURL, path, revision, controllerName, controllerNamespace string) (*ResolvedBundle, error) {
	cloneDir := filepath.Join(r.workDir, "bundles", controllerName)
	if err := os.RemoveAll(cloneDir); err != nil {
		return nil, fmt.Errorf("clean clone dir: %w", err)
	}

	mat, err := r.Materialize(context.Background(), repoURL, path, revision, cloneDir, nil)
	if err != nil {
		return nil, err
	}

	// Build complete variable set with varroa_* auto-vars (highest priority).
	vars := make(Variables)
	for k, v := range mat.Variables {
		vars[k] = v
	}
	vars["varroa_controller_name"] = controllerName
	vars["varroa_controller_namespace"] = controllerNamespace
	vars["varroa_controller_endpoint"] = fmt.Sprintf("http://%s-svc.%s.svc.cluster.local:8080", controllerName, controllerNamespace)
	if r.oidcIssuer != "" {
		vars["varroa_oidc_issuer"] = r.oidcIssuer
		vars["varroa_oidc_client_id"] = r.oidcClientID
	}

	// Resolve variables in all content
	return &ResolvedBundle{
		JenkinsYAML: ResolveVars(mat.JenkinsYAML, vars),
		PluginsYAML: ResolveVars(mat.PluginsYAML, vars),
		ItemsYAML:   ResolveVars(mat.ItemsYAML, vars),
		RbacYAML:    ResolveVars(mat.RbacYAML, vars),
		Variables:   vars,
	}, nil
}

// ResolveVars substitutes ${var} placeholders in content using the given
// variables map. Returns the resolved string.
func ResolveVars(content string, vars Variables) string {
	if content == "" {
		return ""
	}
	result := content
	for k, v := range vars {
		placeholder := fmt.Sprintf("${%s}", k)
		result = strings.ReplaceAll(result, placeholder, v)
	}
	return result
}

// InjectedVariableNames are every ${var} name the control plane can auto-inject
// at controller resolve time (see internal/controller/controller_controller.go).
var InjectedVariableNames = []string{
	"varroa_controller_name", "varroa_controller_namespace", "varroa_controller_endpoint",
	"varroa_controller_external_url", "varroa_controller_path_prefix", "varroa_frontend_url",
	"varroa_oidc_issuer", "varroa_oidc_client_id", "varroa_login_url",
}
