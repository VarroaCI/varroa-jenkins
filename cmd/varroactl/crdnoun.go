package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// crdNounOpts configures a generic CRD noun family (get/describe/create/edit/delete).
type crdNounOpts struct {
	Noun    string   // command name (singular)
	Aliases []string // e.g. []string{"roles"}
	Path    string   // API path prefix, e.g. "/roles"
	// Cluster-scoped nouns reject -n and never prefix ns/ in -o name output.
	Namespaced bool
	// ClusterScoped means the noun lives under /clusters/{cluster}/ (DP3).
	// When true, commands get a --cluster flag.
	ClusterScoped bool
	// Columns returns the table cells for a single item.
	Columns func(item map[string]any) []string
	// Headers is the table header row.
	Headers []string
	// DescribeFrom, when true, registers a "describe" subcommand.
	DescribeFrom bool
}

// crdNounPath returns the API path for a noun, optionally prefixed with
// /clusters/<cluster> for cluster-scoped nouns.
func crdNounPath(opts crdNounOpts, cluster string) string {
	if opts.ClusterScoped && cluster != "" {
		return "/clusters/" + cluster + opts.Path
	}
	return opts.Path
}

// resolveCrdCluster reads the --cluster flag and resolves it via resolveCluster.
func resolveCrdCluster(cmd *cobra.Command) string {
	cFlag, _ := cmd.Flags().GetString("cluster")
	rc, err := resolveContext(func(n string) string {
		f := cmd.Flag(n)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return resolveCluster(cFlag, "")
	}
	return resolveCluster(cFlag, rc.defaultCluster)
}

// addCRDNoun registers get/describe/create/edit/delete subcommands under the
// verb parents in v for the noun described by opts.
func addCRDNoun(v *verbCommands, opts crdNounOpts) {
	// --- get ---
	getCmd := &cobra.Command{
		Use:     opts.Noun,
		Aliases: opts.Aliases,
		Short:   fmt.Sprintf("Get %s(s)", opts.Noun),
		RunE:    makeRunGet(opts),
	}
	if opts.ClusterScoped {
		addClusterFlag(getCmd)
	}
	v.get.AddCommand(getCmd)

	// --- describe ---
	if opts.DescribeFrom {
		descCmd := &cobra.Command{
			Use:     opts.Noun + " NAME",
			Aliases: opts.Aliases,
			Short:   fmt.Sprintf("Describe a %s", opts.Noun),
			Args:    cobra.ExactArgs(1),
			RunE:    makeRunDescribe(opts),
		}
		if opts.ClusterScoped {
			addClusterFlag(descCmd)
		}
		v.describe.AddCommand(descCmd)
	}

	// --- create ---
	createCmd := &cobra.Command{
		Use:     opts.Noun + " -f FILE|-",
		Aliases: opts.Aliases,
		Short:   fmt.Sprintf("Create a %s from a YAML file", opts.Noun),
		RunE:    makeRunCreate(opts),
	}
	createCmd.Flags().StringP("file", "f", "", "YAML file (or - for stdin)")
	_ = createCmd.MarkFlagRequired("file")
	if opts.ClusterScoped {
		addClusterFlag(createCmd)
	}
	v.create.AddCommand(createCmd)

	// --- edit ---
	editCmd := &cobra.Command{
		Use:     opts.Noun + " NAME",
		Aliases: opts.Aliases,
		Short:   fmt.Sprintf("Edit a %s in $EDITOR", opts.Noun),
		Args:    cobra.ExactArgs(1),
		RunE:    makeRunEdit(opts),
	}
	if opts.ClusterScoped {
		addClusterFlag(editCmd)
	}
	v.edit.AddCommand(editCmd)

	// --- delete ---
	deleteCmd := &cobra.Command{
		Use:     opts.Noun + " NAME",
		Aliases: opts.Aliases,
		Short:   fmt.Sprintf("Delete a %s", opts.Noun),
		Args:    cobra.ExactArgs(1),
		RunE:    makeRunDelete(opts),
	}
	if opts.ClusterScoped {
		addClusterFlag(deleteCmd)
	}
	v.delete.AddCommand(deleteCmd)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// resolveNounArgs resolves name/namespace from args for a noun command.
// For namespaced nouns, uses resolveNSName. For cluster-scoped, rejects -n.
func resolveNounArgs(opts crdNounOpts, cmd *cobra.Command, arg string) (ns string, name string, err error) {
	if opts.Namespaced {
		nFlag, _ := cmd.Flags().GetString("namespace")
		rc, cerr := resolveContext(func(n string) string {
			f := cmd.Flag(n)
			if f == nil {
				return ""
			}
			return f.Value.String()
		})
		if cerr != nil {
			return "", "", cerr
		}
		return resolveNSName(arg, nFlag, rc.defaultNamespace)
	}

	// Cluster-scoped
	nFlag, _ := cmd.Flags().GetString("namespace")
	if nFlag != "" {
		return "", "", usagef("--namespace/-n is not supported for cluster-scoped %s", opts.Noun)
	}
	if strings.Contains(arg, "/") {
		parts := strings.SplitN(arg, "/", 2)
		arg = parts[1]
	}
	return "", arg, nil
}

// rawRequest performs an authenticated HTTP request to the API server.
// The path is prefixed with /api/v1 automatically.
func rawRequest(cmd *cobra.Command, method, path string, body []byte) (*http.Response, error) {
	rc, err := resolveContext(func(n string) string {
		f := cmd.Flag(n)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return nil, err
	}

	fullURL := strings.TrimRight(rc.server, "/") + "/api/v1" + path
	var reqBody io.Reader
	if body != nil {
		reqBody = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rc.apiKey)
	req.Header.Set("User-Agent", "varroactl/"+version)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	return http.DefaultClient.Do(req)
}

// readStdinOrFile reads data from a file or stdin ("-").
func readStdinOrFile(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// itemName extracts the name from a JSON-like item map.
func itemName(item map[string]any) string {
	if meta, ok := item["metadata"].(map[string]any); ok {
		if n, ok := meta["name"].(string); ok {
			return n
		}
	}
	if n, ok := item["name"].(string); ok {
		return n
	}
	return "<unknown>"
}

// ---------------------------------------------------------------------------
// List helper
// ---------------------------------------------------------------------------

func listNoun(opts crdNounOpts, cmd *cobra.Command, format string, noHeaders bool) error {
	basePath := crdNounPath(opts, resolveCrdCluster(cmd))
	url := basePath
	if opts.Namespaced {
		nFlag, _ := cmd.Flags().GetString("namespace")
		aFlag, _ := cmd.Flags().GetBool("all-namespaces")
		rc, err := resolveContext(func(n string) string {
			f := cmd.Flag(n)
			if f == nil {
				return ""
			}
			return f.Value.String()
		})
		if err != nil {
			return err
		}
		ns := resolveListNamespace(nFlag, aFlag, rc.defaultNamespace)
		if ns != "" {
			url = basePath + "?namespace=" + ns
		}
	}

	httpResp, err := rawRequest(cmd, "GET", url, nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var envelope struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("failed to decode list: %w", err)
	}

	switch format {
	case "json":
		return printJSON(os.Stdout, envelope.Items)
	case "yaml":
		return printYAML(os.Stdout, envelope.Items)
	case "name":
		for _, item := range envelope.Items {
			n := itemName(item)
			if opts.Namespaced {
				if ns, ok := item["namespace"].(string); ok && ns != "" {
					n = ns + "/" + n
				}
			}
			_, _ = fmt.Fprintln(os.Stdout, n)
		}
		return nil
	case "table", "wide":
		rows := make([][]string, 0, len(envelope.Items))
		for _, item := range envelope.Items {
			rows = append(rows, opts.Columns(item))
		}
		printTable(os.Stdout, opts.Headers, rows, noHeaders)
		return nil
	}
	return nil
}

// ---------------------------------------------------------------------------
// Single item fetch
// ---------------------------------------------------------------------------

func fetchSingle(opts crdNounOpts, cmd *cobra.Command, arg string) (map[string]any, error) {
	ns, name, err := resolveNounArgs(opts, cmd, arg)
	if err != nil {
		return nil, err
	}
	basePath := crdNounPath(opts, resolveCrdCluster(cmd))
	url := basePath + "/" + name
	if ns != "" {
		url = basePath + "/" + ns + "/" + name
	}

	httpResp, err := rawRequest(cmd, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return nil, errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return doc, nil
}

// ---------------------------------------------------------------------------
// Command runner factories
// ---------------------------------------------------------------------------

func makeRunGet(opts crdNounOpts) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// Check cluster-scoped -n before anything else
		if !opts.Namespaced {
			nFlag, _ := cmd.Flags().GetString("namespace")
			if nFlag != "" {
				return usagef("--namespace/-n is not supported for cluster-scoped %s", opts.Noun)
			}
		}

		if _, err := apiClient(cmd); err != nil {
			return err
		}
		o, _ := cmd.Flags().GetString("output")
		noHeaders, _ := cmd.Flags().GetBool("no-headers")

		if len(args) == 1 {
			doc, err := fetchSingle(opts, cmd, args[0])
			if err != nil {
				return err
			}
			ns, _, _ := resolveNounArgs(opts, cmd, args[0])
			switch o {
			case "json":
				return printJSON(os.Stdout, doc)
			case "yaml":
				return printYAML(os.Stdout, doc)
			case "name":
				n := itemName(doc)
				if opts.Namespaced && ns != "" {
					n = ns + "/" + n
				}
				_, _ = fmt.Fprintln(os.Stdout, n)
				return nil
			case "table", "wide":
				row := opts.Columns(doc)
				printTable(os.Stdout, opts.Headers, [][]string{row}, false)
				return nil
			}
			return nil
		}
		return listNoun(opts, cmd, o, noHeaders)
	}
}

func makeRunDescribe(opts crdNounOpts) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		o, _ := cmd.Flags().GetString("output")

		doc, err := fetchSingle(opts, cmd, args[0])
		if err != nil {
			return err
		}

		switch o {
		case "json":
			return printJSON(os.Stdout, doc)
		case "yaml":
			return printYAML(os.Stdout, doc)
		default:
			return printDescribe(os.Stdout, doc)
		}
	}
}

func makeRunCreate(opts crdNounOpts) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		// Check cluster-scoped -n before anything else
		if !opts.Namespaced {
			nFlag, _ := cmd.Flags().GetString("namespace")
			if nFlag != "" {
				return usagef("--namespace/-n is not supported for cluster-scoped %s", opts.Noun)
			}
		}

		f, _ := cmd.Flags().GetString("file")
		data, err := readStdinOrFile(f)
		if err != nil {
			return err
		}
		var body map[string]any
		if err := yaml.Unmarshal(data, &body); err != nil {
			return fmt.Errorf("failed to parse YAML: %w", err)
		}

		// Build URL
		basePath := crdNounPath(opts, resolveCrdCluster(cmd))
		url := basePath
		if opts.Namespaced {
			ns := resolveCreateNS(cmd, body)
			if ns != "" {
				url = basePath + "/" + ns
			}
		}

		jsonBody, _ := json.Marshal(body)
		httpResp, err := rawRequest(cmd, "POST", url, jsonBody)
		if err != nil {
			return err
		}
		defer func() { _ = httpResp.Body.Close() }()

		if httpResp.StatusCode >= 400 {
			b, _ := io.ReadAll(httpResp.Body)
			return errFromResponse(b, httpResp.StatusCode)
		}

		b, _ := io.ReadAll(httpResp.Body)
		if len(b) > 0 {
			_, _ = fmt.Fprintln(os.Stdout, string(b))
		}
		return nil
	}
}

func makeRunEdit(opts crdNounOpts) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ns, name, err := resolveNounArgs(opts, cmd, args[0])
		if err != nil {
			return err
		}
		basePath := crdNounPath(opts, resolveCrdCluster(cmd))

		return editLoop(
			func() (map[string]any, error) {
				url := basePath + "/" + name
				if ns != "" {
					url = basePath + "/" + ns + "/" + name
				}
				httpResp, err := rawRequest(cmd, "GET", url, nil)
				if err != nil {
					return nil, err
				}
				defer func() { _ = httpResp.Body.Close() }()
				if httpResp.StatusCode >= 400 {
					b, _ := io.ReadAll(httpResp.Body)
					return nil, errFromResponse(b, httpResp.StatusCode)
				}
				body, _ := io.ReadAll(httpResp.Body)
				var doc map[string]any
				if err := json.Unmarshal(body, &doc); err != nil {
					return nil, fmt.Errorf("failed to decode: %w", err)
				}
				return doc, nil
			},
			func(doc map[string]any) error {
				url := basePath + "/" + name
				if ns != "" {
					url = basePath + "/" + ns + "/" + name
				}
				jsonBody, _ := json.Marshal(doc)
				httpResp, err := rawRequest(cmd, "PUT", url, jsonBody)
				if err != nil {
					return err
				}
				defer func() { _ = httpResp.Body.Close() }()
				if httpResp.StatusCode >= 400 {
					b, _ := io.ReadAll(httpResp.Body)
					return errFromResponse(b, httpResp.StatusCode)
				}
				return nil
			},
			opts.Noun, name,
		)
	}
}

func makeRunDelete(opts crdNounOpts) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		ns, name, err := resolveNounArgs(opts, cmd, args[0])
		if err != nil {
			return err
		}
		basePath := crdNounPath(opts, resolveCrdCluster(cmd))
		url := basePath + "/" + name
		if ns != "" {
			url = basePath + "/" + ns + "/" + name
		}

		httpResp, err := rawRequest(cmd, "DELETE", url, nil)
		if err != nil {
			return err
		}
		defer func() { _ = httpResp.Body.Close() }()

		if httpResp.StatusCode >= 400 {
			b, _ := io.ReadAll(httpResp.Body)
			return errFromResponse(b, httpResp.StatusCode)
		}

		fmt.Printf("%s %q deleted\n", opts.Noun, args[0])
		return nil
	}
}

// resolveCreateNS determines the namespace for a create operation.
func resolveCreateNS(cmd *cobra.Command, body map[string]any) string {
	nFlag, _ := cmd.Flags().GetString("namespace")
	if nFlag != "" {
		return nFlag
	}
	if m, ok := body["metadata"].(map[string]any); ok {
		if ns, ok := m["namespace"].(string); ok && ns != "" {
			return ns
		}
	}
	rc, err := resolveContext(func(n string) string {
		f := cmd.Flag(n)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err == nil {
		return rc.defaultNamespace
	}
	return ""
}
