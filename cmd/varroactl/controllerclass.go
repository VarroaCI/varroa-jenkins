package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func init() {
	registerRootCommand(func(root *cobra.Command) {
		ccGet := &cobra.Command{
			Use:     "controllerclasses [NAME]",
			Aliases: []string{"controllerclass", "cc"},
			Short:   "Get controller classes",
			Args:    cobra.MaximumNArgs(1),
			RunE:    runGetControllerClasses,
		}
		addClusterFlag(ccGet)
		findCommand(root, "get").AddCommand(ccGet)

		ccDesc := &cobra.Command{
			Use:     "controllerclass NAME",
			Aliases: []string{"controllerclasses", "cc"},
			Short:   "Describe a controller class",
			Args:    cobra.ExactArgs(1),
			RunE:    runDescribeControllerClass,
		}
		addClusterFlag(ccDesc)
		findCommand(root, "describe").AddCommand(ccDesc)
	})
}

func controllerClassColumns(item map[string]any) []string {
	name := itemName(item)

	// Count non-empty spec fields for a summary column.
	spec := "-"
	if s, ok := item["spec"].(map[string]any); ok {
		count := 0
		for k, v := range s {
			if k == "mite" {
				if m, ok := v.(map[string]any); ok {
					for range m {
						count++
					}
				}
				continue
			}
			switch val := v.(type) {
			case string:
				if val != "" {
					count++
				}
			case map[string]any:
				if len(val) > 0 {
					count++
				}
			case []any:
				if len(val) > 0 {
					count++
				}
			}
		}
		spec = fmt.Sprintf("%d fields", count)
	}

	// IngressClassName is the most interesting single field.
	ingressClass := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if ic, ok := spec["ingressClassName"].(string); ok {
			ingressClass = ic
		}
	}

	return []string{name, spec, ingressClass}
}

func runGetControllerClasses(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	httpResp, err := rawRequest(cmd, "GET", "/clusters/"+resolveCrdCluster(cmd)+"/controller-classes", nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	items := env.Items

	// If NAME arg provided, client-side filter
	if len(args) > 0 {
		var filtered []map[string]any
		for _, item := range items {
			if itemName(item) == args[0] {
				filtered = append(filtered, item)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("error from server (404): controllerclass %q not found", args[0])
		}
		if o == "json" || o == "yaml" || o == "name" {
			return renderSingle(filtered[0], o, controllerClassColumns,
				[]string{"NAME", "SPEC", "INGRESS-CLASS"})
		}
		// table: single row
		row := controllerClassColumns(filtered[0])
		printTable(os.Stdout, []string{"NAME", "SPEC", "INGRESS-CLASS"},
			[][]string{row}, noHeaders)
		return nil
	}

	return renderList(items, o, noHeaders, controllerClassColumns,
		[]string{"NAME", "SPEC", "INGRESS-CLASS"})
}

func runDescribeControllerClass(cmd *cobra.Command, args []string) error {
	o, _ := cmd.Flags().GetString("output")

	httpResp, err := rawRequest(cmd, "GET", "/clusters/"+resolveCrdCluster(cmd)+"/controller-classes", nil)
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}

	body, _ := io.ReadAll(httpResp.Body)
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("failed to decode: %w", err)
	}

	name := args[0]
	for _, item := range env.Items {
		if itemName(item) == name {
			switch o {
			case "json":
				return printJSON(os.Stdout, item)
			case "yaml":
				return printYAML(os.Stdout, item)
			default:
				return printDescribe(os.Stdout, item)
			}
		}
	}

	return fmt.Errorf("error from server (404): controllerclass %q not found", name)
}
