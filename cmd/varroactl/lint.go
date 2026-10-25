// Package main: the lint commands (design D13, delta varroa-cli).
//
// `lint marker` checks a pipeline marker file against a tenant's delivered
// pipeline templates before it is committed. The BFF endpoint it calls
// (POST /pipelines/{tenant}/markers/validate) has no generated client method
// yet, so this file posts the request directly, the same way activity.go
// does for a path the generated client does not cover.
//
// `lint jenkinsfile` lints a declarative Jenkinsfile on the tenant's cell.
// It calls the generated client method LintJenkinsfileWithResponse.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// defaultMarkerPath is design pipeline-marker-file's fixed marker
// location, and `lint marker`'s --marker default.
const defaultMarkerPath = ".varroa/pipeline.yaml"

func init() {
	registerRootCommand(registerLintCommands)
}

func registerLintCommands(root *cobra.Command) {
	lintCmd := &cobra.Command{
		Use:   "lint",
		Short: "Lint a file before it is committed",
	}
	root.AddCommand(lintCmd)

	markerCmd := &cobra.Command{
		Use:   "marker",
		Short: "Lint a pipeline marker file against a tenant's delivered templates",
		Long: `Lint a pipeline marker file.

Reads the marker file and posts it to the tenant's markers/validate
endpoint, which resolves it against the tenant's delivered pipeline
templates and runs the Go validator. It contacts no cell and no record
store, so it works while a cell is down and while the record store is
disabled.

Exits zero when the findings list is empty. Exits non-zero when the
list holds any finding.`,
		RunE: runLintMarker,
	}
	markerCmd.Flags().String("tenant", "", "tenant the marker belongs to (required)")
	markerCmd.Flags().String("marker", defaultMarkerPath, "path to the marker file")
	lintCmd.AddCommand(markerCmd)

	jenkinsfileCmd := &cobra.Command{
		Use:   "jenkinsfile",
		Short: "Lint a declarative Jenkinsfile on a tenant's cell",
		Long: `Lint a declarative Jenkinsfile.

Reads the file and posts it to the tenant's lint endpoint, which runs the
Jenkins declarative linter on the tenant's cell and the Varroa cell rules.
The lint writes no state and never wakes a hibernated cell.

Exits zero when the file is valid. Exits non-zero when the linter reports
any error, or when the server refuses the request.`,
		RunE: runLintJenkinsfile,
	}
	jenkinsfileCmd.Flags().String("tenant", "", "tenant the Jenkinsfile belongs to (required)")
	jenkinsfileCmd.Flags().String("file", defaultJenkinsfilePath, "path to the Jenkinsfile")
	lintCmd.AddCommand(jenkinsfileCmd)
}

// defaultJenkinsfilePath is `lint jenkinsfile`'s --file default.
const defaultJenkinsfilePath = "Jenkinsfile"

// errJenkinsfileInvalid is returned when the lint reports any error, so
// `run` exits non-zero after the findings have already printed.
var errJenkinsfileInvalid = errors.New("jenkinsfile failed lint")

func runLintJenkinsfile(cmd *cobra.Command, args []string) error {
	tenant, _ := cmd.Flags().GetString("tenant")
	if tenant == "" {
		return usagef("--tenant is required")
	}
	path, _ := cmd.Flags().GetString("file")

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read jenkinsfile %q: %w", path, err)
	}

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	body := client.LintJenkinsfileJSONRequestBody{Jenkinsfile: string(raw)}
	resp, err := c.LintJenkinsfileWithResponse(cmd.Context(), tenant, body)
	if err != nil {
		return err
	}
	if resp.HTTPResponse.StatusCode >= 400 || resp.JSON200 == nil {
		return errFromResponse(resp.Body, resp.StatusCode())
	}

	if err := printJenkinsfileLint(cmd, resp.JSON200.Errors); err != nil {
		return err
	}
	if !resp.JSON200.Valid {
		return errJenkinsfileInvalid
	}
	return nil
}

// printJenkinsfileLint honours the shared -o {table,wide,json,yaml,name}
// contract. Table columns are SOURCE, LINE, COLUMN, and MESSAGE.
func printJenkinsfileLint(cmd *cobra.Command, findings []client.ComponentsSchemasJenkinsfileLintError) error {
	o, _ := cmd.Flags().GetString("output")
	nh, _ := cmd.Flags().GetBool("no-headers")
	switch o {
	case "json":
		return printJSON(os.Stdout, findings)
	case "yaml":
		return printYAML(os.Stdout, findings)
	case "name":
		for _, f := range findings {
			_, _ = fmt.Fprintln(os.Stdout, lintLocation(f.Line, f.Column))
		}
		return nil
	default: // "table", "wide": no extra columns.
		rows := make([][]string, len(findings))
		for i, f := range findings {
			rows[i] = []string{string(f.Source), lintNumber(f.Line), lintNumber(f.Column), f.Message}
		}
		printTable(os.Stdout, []string{"SOURCE", "LINE", "COLUMN", "MESSAGE"}, rows, nh)
		return nil
	}
}

// lintNumber renders a line or column, or "-" when the linter gave none.
func lintNumber(n *int) string {
	if n == nil {
		return "-"
	}
	return strconv.Itoa(*n)
}

// lintLocation renders a finding's line:column for the name output.
func lintLocation(line, column *int) string {
	if line == nil || column == nil {
		return "-"
	}
	return fmt.Sprintf("%d:%d", *line, *column)
}

// markerValidateFinding mirrors internal/api's markerValidateFindingJSON.
type markerValidateFinding struct {
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// markerValidateResponse mirrors internal/api's markerValidateResponseJSON.
type markerValidateResponse struct {
	Findings        []markerValidateFinding `json:"findings"`
	TemplateName    string                  `json:"templateName,omitempty"`
	TemplateVersion string                  `json:"templateVersion,omitempty"`
}

// errMarkerFindings is returned when the findings list is non-empty, so
// `run` exits non-zero after the findings table has already printed.
var errMarkerFindings = errors.New("marker failed validation")

func runLintMarker(cmd *cobra.Command, args []string) error {
	tenant, _ := cmd.Flags().GetString("tenant")
	if tenant == "" {
		return usagef("--tenant is required")
	}
	markerPath, _ := cmd.Flags().GetString("marker")

	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return fmt.Errorf("read marker %q: %w", markerPath, err)
	}

	rc, err := resolveContext(func(name string) string {
		f := cmd.Flag(name)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(map[string]string{"marker": string(raw)})
	if err != nil {
		return err
	}

	fullURL := strings.TrimRight(rc.server, "/") + "/api/v1/pipelines/" + tenant + "/markers/validate"
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, fullURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+rc.apiKey)
	req.Header.Set("User-Agent", "varroactl/"+version)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return errFromResponse(respBody, resp.StatusCode)
	}

	var result markerValidateResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decode findings: %w", err)
	}

	if err := printMarkerFindings(cmd, result.Findings); err != nil {
		return err
	}
	if len(result.Findings) > 0 {
		return errMarkerFindings
	}
	return nil
}

// printMarkerFindings honours the shared -o {table,wide,json,yaml,name}
// contract (varroa-cli: table columns SEVERITY, PATH, MESSAGE).
func printMarkerFindings(cmd *cobra.Command, findings []markerValidateFinding) error {
	o, _ := cmd.Flags().GetString("output")
	nh, _ := cmd.Flags().GetBool("no-headers")
	switch o {
	case "json":
		return printJSON(os.Stdout, findings)
	case "yaml":
		return printYAML(os.Stdout, findings)
	case "name":
		for _, f := range findings {
			name := f.Path
			if name == "" {
				name = "-"
			}
			_, _ = fmt.Fprintln(os.Stdout, name)
		}
		return nil
	default: // "table", "wide": no extra columns.
		rows := make([][]string, len(findings))
		for i, f := range findings {
			rows[i] = []string{f.Severity, f.Path, f.Message}
		}
		printTable(os.Stdout, []string{"SEVERITY", "PATH", "MESSAGE"}, rows, nh)
		return nil
	}
}
