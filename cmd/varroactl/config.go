package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// Config types
// ---------------------------------------------------------------------------

type cliConfig struct {
	CurrentContext string       `json:"currentContext"`
	Contexts       []cliContext `json:"contexts"`
}

type cliContext struct {
	Name             string `json:"name"`
	Server           string `json:"server"`
	APIKey           string `json:"apiKey"`
	DefaultNamespace string `json:"defaultNamespace,omitempty"`
	DefaultCluster   string `json:"defaultCluster,omitempty"`
}

// ---------------------------------------------------------------------------
// Injectable GOOS for testing
// ---------------------------------------------------------------------------

var goos = runtime.GOOS

// ---------------------------------------------------------------------------
// Path resolution (design §3)
// ---------------------------------------------------------------------------

func configPath() string {
	if p := os.Getenv("VARROACTL_CONFIG"); p != "" {
		return p
	}
	var base string
	if goos == "windows" {
		ucd, err := os.UserConfigDir()
		if err == nil {
			base = ucd
		}
	} else {
		base = os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err == nil {
				base = filepath.Join(home, ".config")
			}
		}
	}
	if base == "" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".config", "varroactl", "config.yaml")
	}
	return filepath.Join(base, "varroactl", "config.yaml")
}

// ---------------------------------------------------------------------------
// Load / Save
// ---------------------------------------------------------------------------

func loadConfig() (*cliConfig, error) {
	cfg := &cliConfig{}
	p := configPath()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Contexts == nil {
		cfg.Contexts = []cliContext{}
	}
	return cfg, nil
}

func saveConfig(cfg *cliConfig) error {
	p := configPath()
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	// Atomic write: temp file in the same directory, then rename.
	tmp, err := os.CreateTemp(dir, "config-*.yaml")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// ---------------------------------------------------------------------------
// Resolve context (precedence: flags > env > config)
// ---------------------------------------------------------------------------

// resolvedContext holds the effective connection parameters.
type resolvedContext struct {
	server           string
	apiKey           string
	defaultNamespace string
	defaultCluster   string
}

// resolveContext resolves the effective context from flags, env, and config.
// getFlag is a function that returns the string value of a flag (or "").
func resolveContext(getFlag func(name string) string) (*resolvedContext, error) {
	ctxFlag := getFlag("context")
	serverFlag := getFlag("server")

	// Environment variables
	envServer := os.Getenv("VARROACTL_SERVER")
	envAPIKey := os.Getenv("VARROACTL_API_KEY")
	envCtx := os.Getenv("VARROACTL_CONTEXT")

	// Load config
	cfg, err := loadConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	// Determine effective context name: flag > env > config.CurrentContext
	ctxName := ctxFlag
	if ctxName == "" {
		ctxName = envCtx
	}
	if ctxName == "" {
		ctxName = cfg.CurrentContext
	}

	// Find the context entry (may be nil if ctxName is empty)
	var ctx *cliContext
	for i := range cfg.Contexts {
		if cfg.Contexts[i].Name == ctxName {
			ctx = &cfg.Contexts[i]
			break
		}
	}

	// Precedence: flag > env > config
	server := serverFlag
	if server == "" {
		server = envServer
	}
	if server == "" && ctx != nil {
		server = ctx.Server
	}

	apiKey := ""
	if envAPIKey != "" {
		apiKey = envAPIKey
	} else if ctx != nil {
		apiKey = ctx.APIKey
	}

	defaultNS := ""
	if ctx != nil {
		defaultNS = ctx.DefaultNamespace
	}

	defaultCluster := ""
	if ctx != nil {
		defaultCluster = ctx.DefaultCluster
	}

	if server == "" {
		return nil, fmt.Errorf(`no context: run "varroactl login" or set --server`)
	}

	return &resolvedContext{
		server:           strings.TrimRight(server, "/"),
		apiKey:           apiKey,
		defaultNamespace: defaultNS,
		defaultCluster:   defaultCluster,
	}, nil
}

// ---------------------------------------------------------------------------
// Config subcommands
// ---------------------------------------------------------------------------

func addConfigCommand(root *cobra.Command) {
	configCmd := &cobra.Command{
		Use:   "config",
		Short: "Manage CLI contexts and configuration",
	}

	configCmd.AddCommand(
		newGetContextsCmd(),
		newCurrentContextCmd(),
		newUseContextCmd(),
		newSetContextCmd(),
		newSetClusterCmd(),
		newDeleteContextCmd(),
	)

	root.AddCommand(configCmd)
}

func newGetContextsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-contexts",
		Short: "List available contexts",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			w := os.Stdout
			fmt.Fprintf(w, "CURRENT\tNAME\tSERVER\tNAMESPACE\tCLUSTER\n")
			for _, c := range cfg.Contexts {
				mark := ""
				if c.Name == cfg.CurrentContext {
					mark = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mark, c.Name, c.Server, c.DefaultNamespace, c.DefaultCluster)
			}
			return nil
		},
	}
}

func newCurrentContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current-context",
		Short: "Show the current context name",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.CurrentContext == "" {
				return nil
			}
			fmt.Println(cfg.CurrentContext)
			return nil
		},
	}
}

func newUseContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use-context NAME",
		Short: "Set the current context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			found := false
			for _, c := range cfg.Contexts {
				if c.Name == name {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("context %q not found", name)
			}
			cfg.CurrentContext = name
			return saveConfig(cfg)
		},
	}
}

func newSetContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-context NAME [--server URL] [--api-key K] [--namespace NS] [--cluster C]",
		Short: "Create or update a context",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return usagef("NAME is required")
			}
			name := args[0]
			server, _ := cmd.Flags().GetString("server")
			apiKey, _ := cmd.Flags().GetString("api-key")
			ns, _ := cmd.Flags().GetString("namespace")
			cluster, _ := cmd.Flags().GetString("cluster")

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			var existing *cliContext
			for i := range cfg.Contexts {
				if cfg.Contexts[i].Name == name {
					existing = &cfg.Contexts[i]
					break
				}
			}
			if existing == nil {
				cfg.Contexts = append(cfg.Contexts, cliContext{Name: name})
				existing = &cfg.Contexts[len(cfg.Contexts)-1]
			}

			if server != "" {
				existing.Server = server
			}
			if apiKey != "" {
				existing.APIKey = apiKey
			}
			if ns != "" {
				existing.DefaultNamespace = ns
			}
			if cluster != "" {
				existing.DefaultCluster = cluster
			}

			return saveConfig(cfg)
		},
	}
	cmd.Flags().String("server", "", "server URL")
	cmd.Flags().String("api-key", "", "API key")
	cmd.Flags().String("namespace", "", "default namespace")
	cmd.Flags().String("cluster", "", "default cluster")
	return cmd
}

func newSetClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-cluster NAME | set-cluster --unset",
		Short: "Set or clear the default cluster on the current context",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			unset, _ := cmd.Flags().GetBool("unset")
			if (len(args) == 1) == unset { // both or neither
				return usagef("provide exactly one of NAME or --unset")
			}
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			target, _ := cmd.Flags().GetString("context") // inherited persistent root flag
			if target == "" {
				target = cfg.CurrentContext
			}
			if target == "" {
				return fmt.Errorf(`no current context: run "varroactl login" or pass --context`)
			}
			// Find context by name
			var found bool
			for i := range cfg.Contexts {
				if cfg.Contexts[i].Name == target {
					if unset {
						cfg.Contexts[i].DefaultCluster = ""
					} else {
						cfg.Contexts[i].DefaultCluster = args[0]
					}
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("context %q not found", target)
			}
			return saveConfig(cfg)
		},
	}
	cmd.Flags().Bool("unset", false, "clear the default cluster")
	return cmd
}

func newDeleteContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete-context NAME",
		Short: "Delete a context",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			filtered := make([]cliContext, 0, len(cfg.Contexts))
			for _, c := range cfg.Contexts {
				if c.Name != name {
					filtered = append(filtered, c)
				}
			}
			cfg.Contexts = filtered
			if cfg.CurrentContext == name {
				cfg.CurrentContext = ""
			}
			return saveConfig(cfg)
		},
	}
}
