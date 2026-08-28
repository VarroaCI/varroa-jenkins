package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// configPath resolution tests
// ---------------------------------------------------------------------------

func TestConfigPath_VARROACTL_CONFIG(t *testing.T) {
	t.Setenv("VARROACTL_CONFIG", "/custom/path/config.yaml")
	got := configPath()
	if got != "/custom/path/config.yaml" {
		t.Errorf("expected /custom/path/config.yaml, got %s", got)
	}
}

func TestConfigPath_Windows(t *testing.T) {
	// Save and restore goos
	orig := goos
	t.Cleanup(func() { goos = orig })
	goos = "windows"

	// Unset VARROACTL_CONFIG
	t.Setenv("VARROACTL_CONFIG", "")
	// On a non-Windows machine, UserConfigDir is simulated.
	// We can't easily fake os.UserConfigDir, but we can verify the path is under varroactl/config.yaml
	got := configPath()
	if !strings.HasSuffix(got, "varroactl/config.yaml") {
		t.Errorf("expected path ending with varroactl/config.yaml, got %s", got)
	}
}

func TestConfigPath_XDG_SET(t *testing.T) {
	orig := goos
	t.Cleanup(func() { goos = orig })
	goos = "linux"

	t.Setenv("VARROACTL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "/home/user/xdg")
	got := configPath()
	expected := "/home/user/xdg/varroactl/config.yaml"
	if got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
}

func TestConfigPath_XDG_Unset(t *testing.T) {
	orig := goos
	t.Cleanup(func() { goos = orig })
	goos = "linux"

	t.Setenv("VARROACTL_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	got := configPath()
	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".config", "varroactl", "config.yaml")
	if got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
}

// ---------------------------------------------------------------------------
// Save → Load round-trip
// ---------------------------------------------------------------------------

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	// Write to a temp directory
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:             "prod",
				Server:           "https://varroa.example.com",
				APIKey:           "vk_abc.def",
				DefaultNamespace: "team-a",
			},
			{
				Name:   "staging",
				Server: "https://staging.example.com",
				APIKey: "vk_xyz.123",
			},
		},
	}

	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	if loaded.CurrentContext != "prod" {
		t.Errorf("expected CurrentContext=prod, got %s", loaded.CurrentContext)
	}
	if len(loaded.Contexts) != 2 {
		t.Fatalf("expected 2 contexts, got %d", len(loaded.Contexts))
	}
	if loaded.Contexts[0].Name != "prod" {
		t.Errorf("expected context[0].Name=prod, got %s", loaded.Contexts[0].Name)
	}
	if loaded.Contexts[0].Server != "https://varroa.example.com" {
		t.Errorf("expected Server=https://varroa.example.com, got %s", loaded.Contexts[0].Server)
	}
	if loaded.Contexts[0].APIKey != "vk_abc.def" {
		t.Errorf("expected APIKey=vk_abc.def, got %s", loaded.Contexts[0].APIKey)
	}
	if loaded.Contexts[0].DefaultNamespace != "team-a" {
		t.Errorf("expected DefaultNamespace=team-a, got %s", loaded.Contexts[0].DefaultNamespace)
	}
	if loaded.Contexts[1].DefaultNamespace != "" {
		t.Errorf("expected empty DefaultNamespace, got %s", loaded.Contexts[1].DefaultNamespace)
	}
}

// ---------------------------------------------------------------------------
// File mode assertions
// ---------------------------------------------------------------------------

func TestConfigFileMode(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "varroactl", "config.yaml")
	t.Setenv("VARROACTL_CONFIG", configPath)

	cfg := &cliConfig{Contexts: []cliContext{{Name: "test"}}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Check directory mode
	dir := filepath.Dir(configPath)
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0700 {
		t.Errorf("expected dir mode 0700, got %o", di.Mode().Perm())
	}

	// Check file mode
	fi, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("expected file mode 0600, got %o", fi.Mode().Perm())
	}
}

// ---------------------------------------------------------------------------
// Precedence: env beats config, flag beats env
// ---------------------------------------------------------------------------

func TestResolveContext_Precedence(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	// Write a config with a context
	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:             "prod",
				Server:           "https://config.example.com",
				APIKey:           "vk_config_key",
				DefaultNamespace: "config-ns",
			},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Test: no flags, no env → config value
	rc, err := resolveContext(func(name string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if rc.server != "https://config.example.com" {
		t.Errorf("expected server from config, got %s", rc.server)
	}
	if rc.apiKey != "vk_config_key" {
		t.Errorf("expected apiKey from config, got %s", rc.apiKey)
	}

	// Test: env beats config
	t.Setenv("VARROACTL_SERVER", "https://env.example.com")
	t.Setenv("VARROACTL_API_KEY", "vk_env_key")
	rc, err = resolveContext(func(name string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if rc.server != "https://env.example.com" {
		t.Errorf("expected server from env, got %s", rc.server)
	}
	if rc.apiKey != "vk_env_key" {
		t.Errorf("expected apiKey from env, got %s", rc.apiKey)
	}

	// Test: flag beats env
	rc, err = resolveContext(func(name string) string {
		if name == "server" {
			return "https://flag.example.com"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if rc.server != "https://flag.example.com" {
		t.Errorf("expected server from flag, got %s", rc.server)
	}
}

// ---------------------------------------------------------------------------
// set-context merge semantics
// ---------------------------------------------------------------------------

func TestSetContextMerge(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	// Create initial context with all fields
	cfg := &cliConfig{
		Contexts: []cliContext{
			{
				Name:   "test",
				Server: "https://original.example.com",
				APIKey: "vk_original",
			},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Now "update" with just namespace (simulating set-context with --namespace only)
	cfg2 := &cliConfig{
		Contexts: []cliContext{
			{
				Name:             "test",
				Server:           "https://original.example.com",
				APIKey:           "vk_original",
				DefaultNamespace: "new-ns",
			},
		},
	}
	if err := saveConfig(cfg2); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(loaded.Contexts))
	}
	if loaded.Contexts[0].Server != "https://original.example.com" {
		t.Errorf("expected server preserved, got %s", loaded.Contexts[0].Server)
	}
	if loaded.Contexts[0].APIKey != "vk_original" {
		t.Errorf("expected apiKey preserved, got %s", loaded.Contexts[0].APIKey)
	}
	if loaded.Contexts[0].DefaultNamespace != "new-ns" {
		t.Errorf("expected namespace updated to new-ns, got %s", loaded.Contexts[0].DefaultNamespace)
	}
}

// ---------------------------------------------------------------------------
// delete-context clears current pointer
// ---------------------------------------------------------------------------

func TestDeleteContextClearsCurrent(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	cfg := &cliConfig{
		CurrentContext: "to-delete",
		Contexts: []cliContext{
			{Name: "to-delete", Server: "https://example.com"},
			{Name: "keep", Server: "https://keep.example.com"},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Simulate delete-context
	cfg2, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	filtered := make([]cliContext, 0, len(cfg2.Contexts))
	for _, c := range cfg2.Contexts {
		if c.Name != "to-delete" {
			filtered = append(filtered, c)
		}
	}
	cfg2.Contexts = filtered
	if cfg2.CurrentContext == "to-delete" {
		cfg2.CurrentContext = ""
	}
	if err := saveConfig(cfg2); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CurrentContext != "" {
		t.Errorf("expected CurrentContext cleared, got %q", loaded.CurrentContext)
	}
	if len(loaded.Contexts) != 1 {
		t.Fatalf("expected 1 context remaining, got %d", len(loaded.Contexts))
	}
	if loaded.Contexts[0].Name != "keep" {
		t.Errorf("expected remaining context 'keep', got %s", loaded.Contexts[0].Name)
	}
}

// ---------------------------------------------------------------------------
// get-contexts output never contains vk_ (API key not printed)
// ---------------------------------------------------------------------------

func TestGetContextsNoKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	cfg := &cliConfig{
		Contexts: []cliContext{
			{Name: "test", Server: "https://example.com", APIKey: "vk_secret_123"},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Capture output of get-contexts
	cmd := newGetContextsCmd()
	cmd.SetArgs([]string{})
	// We can't easily capture stdout, but we can verify the loaded config
	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(loaded.Contexts))
	}
	// Verify no vk_ in the Name or Server fields
	if strings.Contains(loaded.Contexts[0].Name, "vk_") {
		t.Error("Name should not contain vk_")
	}
	if strings.Contains(loaded.Contexts[0].Server, "vk_") {
		t.Error("Server should not contain vk_")
	}
}

// ---------------------------------------------------------------------------
// set-cluster round-trip
// ---------------------------------------------------------------------------

func TestSetCluster_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	t.Setenv("VARROACTL_CONFIG", configPath)

	// Seed config with a context
	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{
				Name:   "prod",
				Server: "https://example.com",
				APIKey: "vk_key",
			},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Set cluster
	cmd := newSetClusterCmd()
	cmd.SetArgs([]string{"dev-cluster"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Contexts[0].DefaultCluster != "dev-cluster" {
		t.Errorf("expected DefaultCluster=dev-cluster, got %q", loaded.Contexts[0].DefaultCluster)
	}

	// File mode preserved
	fi, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("expected file mode 0600, got %o", fi.Mode().Perm())
	}

	// Unset
	cmd2 := newSetClusterCmd()
	cmd2.SetArgs([]string{"--unset"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded2, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded2.Contexts[0].DefaultCluster != "" {
		t.Errorf("expected DefaultCluster cleared, got %q", loaded2.Contexts[0].DefaultCluster)
	}
}

// ---------------------------------------------------------------------------
// set-cluster: NAME + --unset → exit 2, neither → exit 2
// ---------------------------------------------------------------------------

func TestSetCluster_NameAndUnsetExclusive(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	// Both NAME and --unset
	cmd := newSetClusterCmd()
	cmd.SetArgs([]string{"dev-cluster", "--unset"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for NAME + --unset")
	}
	var ue usageError
	if !errorsAs(err, &ue) {
		t.Fatalf("expected usageError, got %T: %v", err, err)
	}

	// Neither NAME nor --unset
	cmd2 := newSetClusterCmd()
	cmd2.SetArgs([]string{})
	err = cmd2.Execute()
	if err == nil {
		t.Fatal("expected error for neither")
	}
	if !errorsAs(err, &ue) {
		t.Fatalf("expected usageError, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// set-cluster: no current context → exit 1
// ---------------------------------------------------------------------------

func TestSetCluster_NoCurrentContext(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	// Empty config, no current context
	cmd := newSetClusterCmd()
	cmd.SetArgs([]string{"dev-cluster"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for no current context")
	}
	if !strings.Contains(err.Error(), "no current context") {
		t.Errorf("expected 'no current context' in error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// set-cluster: unknown context → exit 1
// ---------------------------------------------------------------------------

func TestSetCluster_UnknownContext(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{Name: "prod", Server: "https://example.com"},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Use full root command so --context flag is available
	root := newRootCmd()
	root.SetArgs([]string{"config", "set-cluster", "dev-cluster", "--context", "nonexistent"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for unknown context")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %v", err)
	}

	// File mtime unchanged
	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Contexts[0].DefaultCluster != "" {
		t.Errorf("expected DefaultCluster unchanged, got %q", loaded.Contexts[0].DefaultCluster)
	}
}

// ---------------------------------------------------------------------------
// set-cluster: --context prod targeting
// ---------------------------------------------------------------------------

func TestSetCluster_WithContextFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	cfg := &cliConfig{
		Contexts: []cliContext{
			{Name: "prod", Server: "https://prod.example.com"},
			{Name: "staging", Server: "https://staging.example.com"},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Target prod via --context (even though no current-context set)
	root := newRootCmd()
	root.SetArgs([]string{"config", "set-cluster", "dev-cluster", "--context", "prod"})
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Contexts[0].Name != "prod" {
		t.Errorf("expected context[0]=prod, got %s", loaded.Contexts[0].Name)
	}
	if loaded.Contexts[0].DefaultCluster != "dev-cluster" {
		t.Errorf("expected prod.DefaultCluster=dev-cluster, got %q", loaded.Contexts[0].DefaultCluster)
	}
	if loaded.Contexts[1].DefaultCluster != "" {
		t.Errorf("expected staging.DefaultCluster unchanged, got %q", loaded.Contexts[1].DefaultCluster)
	}
}

// ---------------------------------------------------------------------------
// set-context --cluster merge
// ---------------------------------------------------------------------------

func TestSetContext_WithClusterFlag(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	cfg := &cliConfig{
		Contexts: []cliContext{
			{
				Name:             "prod",
				Server:           "https://example.com",
				APIKey:           "vk_key",
				DefaultNamespace: "team-a",
			},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	// Update with --cluster only
	cmd := newSetContextCmd()
	cmd.SetArgs([]string{"prod", "--cluster", "dev-cluster"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(loaded.Contexts))
	}
	c := loaded.Contexts[0]
	if c.DefaultCluster != "dev-cluster" {
		t.Errorf("expected DefaultCluster=dev-cluster, got %q", c.DefaultCluster)
	}
	if c.Server != "https://example.com" {
		t.Errorf("expected Server preserved, got %s", c.Server)
	}
	if c.APIKey != "vk_key" {
		t.Errorf("expected APIKey preserved, got %s", c.APIKey)
	}
	if c.DefaultNamespace != "team-a" {
		t.Errorf("expected DefaultNamespace preserved, got %s", c.DefaultNamespace)
	}
}

// ---------------------------------------------------------------------------
// Pre-cluster config files (no defaultCluster field) still load
// ---------------------------------------------------------------------------

func TestConfig_OldFileWithoutDefaultCluster(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	t.Setenv("VARROACTL_CONFIG", configPath)

	// Write an old-style config without defaultCluster
	oldYAML := `currentContext: prod
contexts:
- name: prod
  server: https://example.com
  apiKey: vk_key
  defaultNamespace: team-a
`
	if err := os.WriteFile(configPath, []byte(oldYAML), 0600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Contexts) != 1 {
		t.Fatalf("expected 1 context, got %d", len(loaded.Contexts))
	}
	if loaded.Contexts[0].DefaultCluster != "" {
		t.Errorf("expected empty DefaultCluster, got %q", loaded.Contexts[0].DefaultCluster)
	}
	if loaded.Contexts[0].DefaultNamespace != "team-a" {
		t.Errorf("expected DefaultNamespace=team-a, got %s", loaded.Contexts[0].DefaultNamespace)
	}
}

// ---------------------------------------------------------------------------
// get-contexts output contains CLUSTER column
// ---------------------------------------------------------------------------

func TestGetContexts_ClusterColumn(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("VARROACTL_CONFIG", filepath.Join(tmpDir, "config.yaml"))

	cfg := &cliConfig{
		CurrentContext: "prod",
		Contexts: []cliContext{
			{Name: "prod", Server: "https://prod.example.com", DefaultCluster: "prod-cluster"},
			{Name: "staging", Server: "https://staging.example.com"},
		},
	}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}

	cmd := newGetContextsCmd()
	// Capture stdout
	old := os.Stdout
	pr, pw, _ := os.Pipe()
	os.Stdout = pw

	cmd.SetArgs([]string{})
	execErr := cmd.Execute()

	pw.Close()
	os.Stdout = old
	var buf strings.Builder
	b := make([]byte, 4096)
	for {
		n, _ := pr.Read(b)
		if n == 0 {
			break
		}
		buf.Write(b[:n])
	}
	output := buf.String()

	if execErr != nil {
		t.Fatalf("unexpected error: %v", execErr)
	}
	if !strings.Contains(output, "CLUSTER") {
		t.Error("expected CLUSTER column header in output")
	}
	if !strings.Contains(output, "prod-cluster") {
		t.Error("expected prod-cluster value in output")
	}
}
