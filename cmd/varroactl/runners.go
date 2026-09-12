package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

func init() {
	registerNoun(addRunnerNoun)
}

// runnersNounOpts is the shared crdNounOpts for the "runners" noun. The
// noun's own word is the plural "runners", with the alias "rnr" only: the
// varroa-cli delta forbids the alias "runner", because the top-level
// "runner" command group (runner_register.go) reserves that word. That
// group reuses this noun's verb closures directly, so "runner list" and
// "runner delete" delegate to the exact same generated-client calls as
// "get runners" and "delete runners".
var runnersNounOpts = crdNounOpts{
	Noun:          "runners",
	Aliases:       []string{"rnr"},
	Path:          "/runners",
	Namespaced:    false,
	ClusterScoped: false,
	DescribeFrom:  true,
	Columns:       runnerColumns,
	Headers:       []string{"NAME", "TENANT", "LAUNCHER", "CELL", "CONNECTED", "LAST-SEEN", "AGE"},
	List:          runnerNounList,
	Get:           runnerNounGet,
	Create:        runnerNounCreate,
	Patch:         runnerNounPatch,
	Delete:        runnerNounDelete,
}

// addRunnerNoun registers the "runners" CRD noun (get/describe/create/edit/
// patch/delete) through the generated client. runners is cluster-scoped
// with no cluster segment: the BFF path is /api/v1/runners, like Teams.
func addRunnerNoun(v *verbCommands) {
	addCRDNoun(v, runnersNounOpts)
}

// ---------------------------------------------------------------------------
// Table columns
// ---------------------------------------------------------------------------

func runnerColumns(item map[string]any) []string {
	name := itemName(item)

	tenant := ""
	launcher := ""
	if spec, ok := item["spec"].(map[string]any); ok {
		if t, ok := spec["team"].(string); ok {
			tenant = t
		}
		if l, ok := spec["launcher"].(string); ok {
			launcher = l
		}
	}

	cell := "-"
	connected := "false"
	lastSeen := "-"
	if status, ok := item["status"].(map[string]any); ok {
		if c, ok := status["cell"].(string); ok && c != "" {
			cell = c
		}
		if b, ok := status["connected"].(bool); ok {
			connected = fmt.Sprintf("%v", b)
		}
		if ls, ok := status["lastSeen"].(string); ok && ls != "" {
			lastSeen = ls
		}
	}

	// AGE: the wire shape (design D12, ComponentsSchemasRunnerEntry) carries
	// no creation timestamp, the same gap ComponentsSchemasBroodRunSummaryRow
	// leaves for its own AGE column.
	age := ""

	return []string{name, tenant, launcher, cell, connected, lastSeen, age}
}

// ---------------------------------------------------------------------------
// Generated-client callbacks (design D12, varroa-cli delta)
// ---------------------------------------------------------------------------

func runnerNounList(c *client.ClientWithResponses, cmd *cobra.Command) ([]map[string]any, error) {
	params := &client.ListRunnersParams{}
	if tenant, _ := cmd.Flags().GetString("tenant"); tenant != "" {
		params.Team = &tenant
	}

	resp, err := c.ListRunnersWithResponse(cmd.Context(), params)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() >= 400 {
		return nil, errFromResponse(resp.Body, resp.StatusCode())
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("unexpected response: no data")
	}

	docs := make([]map[string]any, 0, len(resp.JSON200.Items))
	for i := range resp.JSON200.Items {
		doc, err := runnerEntryToDoc(&resp.JSON200.Items[i])
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func runnerNounGet(c *client.ClientWithResponses, cmd *cobra.Command, name string) (map[string]any, error) {
	resp, err := c.GetRunnerWithResponse(cmd.Context(), name)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() >= 400 {
		return nil, errFromResponse(resp.Body, resp.StatusCode())
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("unexpected response: no data")
	}
	return runnerEntryToDoc(resp.JSON200)
}

func runnerNounCreate(c *client.ClientWithResponses, cmd *cobra.Command, body map[string]any) (map[string]any, error) {
	req, err := runnerDocToCreateRequest(body)
	if err != nil {
		return nil, err
	}

	resp, err := c.CreateRunnerWithResponse(cmd.Context(), req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() >= 400 {
		return nil, errFromResponse(resp.Body, resp.StatusCode())
	}
	if resp.JSON201 != nil {
		return map[string]any{"name": resp.JSON201.Name}, nil
	}
	return nil, nil
}

func runnerNounPatch(c *client.ClientWithResponses, cmd *cobra.Command, name string, mergePatch []byte) (map[string]any, error) {
	resp, err := c.PatchRunnerWithBodyWithResponse(cmd.Context(), name,
		"application/merge-patch+json", bytes.NewReader(mergePatch))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode() >= 400 {
		return nil, errFromResponse(resp.Body, resp.StatusCode())
	}
	if resp.JSON200 != nil {
		return map[string]any{"name": resp.JSON200.Name}, nil
	}
	return nil, nil
}

func runnerNounDelete(c *client.ClientWithResponses, cmd *cobra.Command, name string) error {
	resp, err := c.DeleteRunnerWithResponse(cmd.Context(), name)
	if err != nil {
		return err
	}
	if resp.StatusCode() >= 400 {
		return errFromResponse(resp.Body, resp.StatusCode())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Wire-shape conversion
// ---------------------------------------------------------------------------

// runnerEntryToDoc converts the generated client's flat {name, spec, status}
// DTO into the {metadata: {name}, spec, status} document shape crdnoun.go's
// shared machinery (Columns, printDescribe, editLoop) expects of every noun.
func runnerEntryToDoc(entry *client.ComponentsSchemasRunnerEntry) (map[string]any, error) {
	spec, err := toDocMap(entry.Spec)
	if err != nil {
		return nil, err
	}
	status, err := toDocMap(entry.Status)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"metadata": map[string]any{"name": entry.Name},
		"spec":     spec,
		"status":   status,
	}, nil
}

// runnerDocToCreateRequest converts a create/edit document ({metadata:
// {name}, spec}, the shape a YAML file or an edited buffer carries) into the
// generated client's CreateRunner request body.
func runnerDocToCreateRequest(body map[string]any) (client.CreateRunnerJSONRequestBody, error) {
	name := docName(body)
	if name == "" {
		return client.CreateRunnerJSONRequestBody{}, usagef("metadata.name is required")
	}

	specJSON, err := json.Marshal(body["spec"])
	if err != nil {
		return client.CreateRunnerJSONRequestBody{}, err
	}
	var spec client.ComponentsSchemasRunnerSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return client.CreateRunnerJSONRequestBody{}, fmt.Errorf("invalid spec: %w", err)
	}

	return client.ComponentsSchemasRunnerCreateRequest{Name: name, Spec: spec}, nil
}

// docName reads metadata.name, falling back to a top-level name key.
func docName(body map[string]any) string {
	if meta, ok := body["metadata"].(map[string]any); ok {
		if n, ok := meta["name"].(string); ok {
			return n
		}
	}
	if n, ok := body["name"].(string); ok {
		return n
	}
	return ""
}

// toDocMap round-trips a typed value through JSON into a generic map, so
// the shared crdnoun.go machinery can walk it the same way it walks a
// rawRequest response.
func toDocMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
