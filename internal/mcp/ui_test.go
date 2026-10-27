package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/api"
	"github.com/varroaci/varroa-jenkins/internal/auth"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

const testDashboardURL = "https://dash.example.com/"

// uiViewMetaTools is the set of tools that advertise a view. The table is
// the same as uiViewTools; the test names it again so a drift shows here.
var uiViewMetaTools = []string{"get_cell_usage", "get_controller", "get_run", "list_fleet_locks"}

func TestUIViewTableMatchesEmbeddedFiles(t *testing.T) {
	matches, err := fs.Glob(uiFS, "ui/*.html")
	if err != nil {
		t.Fatal(err)
	}
	embedded := make([]string, 0, len(matches))
	for _, m := range matches {
		embedded = append(embedded, strings.TrimSuffix(strings.TrimPrefix(m, "ui/"), ".html"))
	}
	want := append([]string(nil), uiViewTools...)
	sort.Strings(embedded)
	sort.Strings(want)
	if strings.Join(embedded, ",") != strings.Join(want, ",") {
		t.Fatalf("embedded views = %v, uiViewTools = %v", embedded, want)
	}
}

func TestUIViewToolsAdvertiseResourceURI(t *testing.T) {
	resp := mcpRequest(t, NewHandler(newTestDeps()), "tools/list", nil, nil)
	if resp.Error != nil {
		t.Fatalf("tools/list returned error: %s", resp.Error)
	}
	var listed struct {
		Tools []struct {
			Name string         `json:"name"`
			Meta map[string]any `json:"_meta"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &listed); err != nil {
		t.Fatal(err)
	}
	withView := map[string]string{}
	for _, tool := range listed.Tools {
		if _, flat := tool.Meta["ui/resourceUri"]; flat {
			t.Errorf("%s carries the deprecated flat ui/resourceUri key", tool.Name)
		}
		ui, ok := tool.Meta["ui"].(map[string]any)
		if !ok {
			continue
		}
		uri, _ := ui["resourceUri"].(string)
		withView[tool.Name] = uri
	}
	if len(withView) != len(uiViewMetaTools) {
		t.Fatalf("tools with a view = %v, want exactly %v", withView, uiViewMetaTools)
	}
	for _, name := range uiViewMetaTools {
		if got, want := withView[name], uiResourceURI(name); got != want {
			t.Errorf("%s resourceUri = %q, want %q", name, got, want)
		}
	}
}

func TestUIViewResourcesRead(t *testing.T) {
	handler := NewHandler(newTestDeps())

	list := mcpRequest(t, handler, "resources/list", nil, nil)
	var listed struct {
		Resources []struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(list.Result, &listed); err != nil {
		t.Fatal(err)
	}
	listedMIME := map[string]string{}
	for _, r := range listed.Resources {
		listedMIME[r.URI] = r.MIMEType
	}

	for _, tool := range uiViewMetaTools {
		uri := uiResourceURI(tool)
		if listedMIME[uri] != uiMIMEType {
			t.Errorf("resources/list lacks %s with %s", uri, uiMIMEType)
		}
		// No claims: the view document is public.
		read := mcpRequest(t, handler, "resources/read", map[string]interface{}{"uri": uri}, nil)
		if read.Error != nil {
			t.Fatalf("resources/read %s returned error: %s", uri, read.Error)
		}
		var got struct {
			Contents []struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(read.Result, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Contents) != 1 {
			t.Fatalf("%s: %d contents, want 1", uri, len(got.Contents))
		}
		c := got.Contents[0]
		if c.MIMEType != uiMIMEType {
			t.Errorf("%s mimeType = %q, want %q", uri, c.MIMEType, uiMIMEType)
		}
		if !strings.HasPrefix(c.Text, "<!doctype html>") {
			t.Errorf("%s text does not start with the doctype", uri)
		}
	}
}

// viewSource reads one embedded view document.
func viewSource(t *testing.T, tool string) string {
	t.Helper()
	b, err := uiFS.ReadFile("ui/" + tool + ".html")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUIViewHTMLForbiddenPatterns(t *testing.T) {
	forbidden := []string{"http://", "https://", "<link", " src=", "@import", "fetch(", "xmlhttprequest", "innerhtml", "n8s"}
	for _, tool := range uiViewTools {
		low := strings.ToLower(viewSource(t, tool))
		for _, pat := range forbidden {
			if strings.Contains(low, pat) {
				t.Errorf("ui/%s.html contains %q", tool, pat)
			}
		}
	}
}

// shellSegments returns the text between each shell marker pair, in order.
func shellSegments(t *testing.T, text string) []string {
	t.Helper()
	const begin, end = "/* shell:begin */", "/* shell:end */"
	var segs []string
	for {
		i := strings.Index(text, begin)
		if i < 0 {
			return segs
		}
		text = text[i+len(begin):]
		j := strings.Index(text, end)
		if j < 0 {
			t.Fatal("shell:begin without shell:end")
		}
		segs = append(segs, text[:j])
		text = text[j+len(end):]
	}
}

func TestUIViewShellIsShared(t *testing.T) {
	var first []string
	for i, tool := range uiViewTools {
		segs := shellSegments(t, viewSource(t, tool))
		if len(segs) != 2 {
			t.Fatalf("ui/%s.html has %d shell segments, want 2", tool, len(segs))
		}
		if i == 0 {
			first = segs
			continue
		}
		for k := range segs {
			if segs[k] != first[k] {
				t.Errorf("ui/%s.html shell segment %d differs from %s", tool, k, uiViewTools[0])
			}
		}
	}
}

func TestUIViewHTMLSpeaksTheHandshake(t *testing.T) {
	needles := []string{
		`"ui/initialize"`, `"ui/notifications/initialized"`, `"ui/notifications/tool-result"`,
		`"ui/notifications/size-changed"`, `"ui/open-link"`, `"2026-01-26"`,
	}
	for _, tool := range uiViewTools {
		src := viewSource(t, tool)
		for _, n := range needles {
			if !strings.Contains(src, n) {
				t.Errorf("ui/%s.html lacks %s", tool, n)
			}
		}
		if !strings.Contains(src, "getFullYear() < 1970") {
			t.Errorf("ui/%s.html lacks the pre-1970 date guard in fmtTime", tool)
		}
		if want := `var TOOL = "` + tool + `";`; !strings.Contains(src, want) {
			t.Errorf("ui/%s.html lacks %s", tool, want)
		}
	}
}

// metaLink returns _meta.dashboardLink of a result, or "" when absent.
func metaLink(tr toolResult) string {
	link, _ := tr.Meta["dashboardLink"].(string)
	return link
}

func TestGetRunViewMetaLink(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED"}
	deps := &api.Dependencies{
		Authorizer:   pipelinesTestAuthorizer("read"),
		RecordStore:  reader,
		DashboardURL: testDashboardURL,
	}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	want := "https://dash.example.com/pipelines/team-a/team-a%2Fbuild/run-a"
	if got := metaLink(tr); got != want {
		t.Fatalf("dashboardLink = %q, want %q", got, want)
	}
}

// The controller-tenant case is checked through the path helpers. The test
// authorizer grants team-a only, so the handler cannot read a ns/ctl run.
func TestGetRunViewControllerTenantPath(t *testing.T) {
	ref, err := api.TenantRefFromRow("", "", "", "ns/ctl")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := api.DashboardRunPath(ref.TenantName, "build", "run-a"), "/pipelines/ctl/build/run-a"; got != want {
		t.Fatalf("run path = %q, want %q", got, want)
	}
}

func TestGetRunViewNoMetaWithoutDashboardURL(t *testing.T) {
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED"}
	deps := &api.Dependencies{Authorizer: pipelinesTestAuthorizer("read"), RecordStore: reader}

	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "run-a"})
	if tr.IsError {
		t.Fatalf("get_run returned error: %v", tr.Content)
	}
	if tr.Meta != nil {
		t.Fatalf("_meta = %v, want none without a dashboard URL", tr.Meta)
	}
}

func TestGetRunErrorResultHasNoMeta(t *testing.T) {
	deps := &api.Dependencies{
		Authorizer:   pipelinesTestAuthorizer("read"),
		RecordStore:  newFakePipelineReader(),
		DashboardURL: testDashboardURL,
	}
	tr := callPipelineTool(t, deps, "get_run", map[string]interface{}{"run": "missing"})
	if !tr.IsError {
		t.Fatal("expected get_run of an unknown run to return an error")
	}
	if tr.Meta != nil {
		t.Fatalf("error result carries _meta %v", tr.Meta)
	}
}

func TestListFleetLocksViewMetaLink(t *testing.T) {
	reader := newFakePipelineReader()
	reader.locksByTenant["team-a"] = []recordstore.FleetLock{
		{Name: "team-a/deploy", RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", State: "held"},
	}
	deps := &api.Dependencies{
		Authorizer:   pipelinesTestAuthorizer("read"),
		RecordStore:  reader,
		DashboardURL: testDashboardURL,
		TenantResolver: &fakePipelineTenantResolver{
			refs: []api.TenantRef{{Tenant: "team-a", TenantName: "team-a", Folder: "team-a"}},
		},
	}
	tr := callPipelineTool(t, deps, "list_fleet_locks", map[string]interface{}{"tenant": "team-a"})
	if tr.IsError {
		t.Fatalf("list_fleet_locks returned error: %v", tr.Content)
	}
	if got, want := metaLink(tr), "https://dash.example.com/pipelines/team-a"; got != want {
		t.Fatalf("dashboardLink = %q, want %q", got, want)
	}
}

// callWithClaims invokes a tool as the given caller and returns the parsed result.
func callWithClaims(t *testing.T, deps *api.Dependencies, claims *auth.Claims, tool string, args map[string]interface{}) toolResult {
	t.Helper()
	resp := mcpRequest(t, NewHandler(deps), "tools/call", map[string]interface{}{
		"name":      tool,
		"arguments": args,
	}, claims)
	return parseToolResult(t, resp.Result)
}

func adminDepsWithDashboard() *api.Dependencies {
	deps := adminDeps()
	deps.DashboardURL = testDashboardURL
	crdstore.MustSeed(deps.Store.(*crdstore.Fake), &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "broken", Namespace: "ns", ResourceVersion: "1"},
		Spec:       v1alpha1.ControllerSpec{Version: "2.516.3"},
		Status: v1alpha1.ControllerStatus{
			Phase: "Failed",
			Conditions: []v1alpha1.ControllerCondition{
				{Type: v1alpha1.ConditionFailed, Status: metav1.ConditionTrue},
			},
		},
	})
	return deps
}

func TestGetControllerViewMeta(t *testing.T) {
	deps := adminDepsWithDashboard()

	tr := callWithClaims(t, deps, mcpAdminClaims, "get_controller", map[string]interface{}{"namespace": "ns", "name": "broken"})
	if tr.IsError {
		t.Fatalf("get_controller returned error: %v", tr.Content)
	}
	if got, want := metaLink(tr), "https://dash.example.com/controllers/core/ns/broken"; got != want {
		t.Fatalf("dashboardLink = %q, want %q", got, want)
	}
	if tr.Meta["kind"] != "stateful" {
		t.Errorf("kind = %v, want stateful", tr.Meta["kind"])
	}
	att, _ := tr.Meta["attention"].(map[string]any)
	if att == nil || att["kind"] != "failed" {
		t.Errorf("attention = %v, want kind failed", tr.Meta["attention"])
	}

	hib := callWithClaims(t, deps, mcpAdminClaims, "get_controller", map[string]interface{}{"namespace": "ns", "name": "hibernated"})
	if hib.IsError {
		t.Fatalf("get_controller hibernated returned error: %v", hib.Meta)
	}
	if _, ok := hib.Meta["attention"]; ok {
		t.Errorf("hibernated controller carries attention %v", hib.Meta["attention"])
	}
}

// cellUsageBrood serves one cell Controller to the cell usage read.
type cellUsageBrood struct {
	api.Brood
	cr *v1alpha1.Controller
}

func (b cellUsageBrood) LocalCluster() string { return "local" }

func (b cellUsageBrood) Get(context.Context, string, string, string) (*v1alpha1.Controller, error) {
	return b.cr, nil
}

// cellUsageReader answers the cell usage read with an empty cell.
type cellUsageReader struct {
	recordstore.Reader
}

func (cellUsageReader) CellUsage(_ context.Context, cluster, namespace, cell string) (recordstore.CellUsage, error) {
	return recordstore.CellUsage{Cluster: cluster, Namespace: namespace, Cell: cell}, nil
}

func cellUsageDeps() *api.Dependencies {
	cell := &v1alpha1.Controller{
		ObjectMeta: metav1.ObjectMeta{Name: "cell-a", Namespace: "ns"},
		Spec: v1alpha1.ControllerSpec{
			Cell: &v1alpha1.CellSpec{Enabled: true, MaxTenants: 4},
		},
	}
	return &api.Dependencies{
		Authorizer:   adminTestAuthorizer(),
		Brood:        cellUsageBrood{cr: cell},
		RecordStore:  cellUsageReader{},
		DashboardURL: testDashboardURL,
	}
}

func TestGetCellUsageViewMeta(t *testing.T) {
	tr := callWithClaims(t, cellUsageDeps(), mcpAdminClaims, "get_cell_usage", map[string]interface{}{"namespace": "ns", "name": "cell-a"})
	if tr.IsError {
		t.Fatalf("get_cell_usage returned error: %v", tr.Content)
	}
	if got, want := metaLink(tr), "https://dash.example.com/controllers/local/ns/cell-a?tab=cell"; got != want {
		t.Fatalf("dashboardLink = %q, want %q", got, want)
	}
}

// TestCallToolDropsResultMeta checks that the in-process host, which the
// assistant and Tenant agents use, forwards the text content only.
func TestCallToolDropsResultMeta(t *testing.T) {
	deps := adminDepsWithDashboard()
	h := NewInProcessHost(NewServer(deps), mcpAdminClaims)
	got, err := h.Call(context.Background(), "get_controller", json.RawMessage(`{"namespace":"ns","name":"broken"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.IsError {
		t.Fatalf("get_controller returned error: %s", got.Content)
	}
	if strings.Contains(got.Content, "dashboardLink") {
		t.Fatalf("in-process result carries view meta: %s", got.Content)
	}
}

// TestViewMetaStaysOutOfText checks that _meta is absent from the text
// content and from structuredContent, so a host without MCP Apps sees the
// same payload as before.
func TestViewMetaStaysOutOfText(t *testing.T) {
	results := map[string]toolResult{
		"get_controller": callWithClaims(t, adminDepsWithDashboard(), mcpAdminClaims, "get_controller",
			map[string]interface{}{"namespace": "ns", "name": "broken"}),
		"get_cell_usage": callWithClaims(t, cellUsageDeps(), mcpAdminClaims, "get_cell_usage",
			map[string]interface{}{"namespace": "ns", "name": "cell-a"}),
	}
	reader := newFakePipelineReader()
	reader.runs["run-a"] = recordstore.Run{RunID: "run-a", Tenant: "team-a", JobPath: "team-a/build", Status: "COMPLETED"}
	results["get_run"] = callPipelineTool(t, &api.Dependencies{
		Authorizer:   pipelinesTestAuthorizer("read"),
		RecordStore:  reader,
		DashboardURL: testDashboardURL,
	}, "get_run", map[string]interface{}{"run": "run-a"})

	for name, tr := range results {
		if tr.IsError {
			t.Fatalf("%s returned error: %v", name, tr.Content)
		}
		if metaLink(tr) == "" {
			t.Errorf("%s carries no dashboardLink in _meta", name)
		}
		if len(tr.Content) == 0 {
			t.Fatalf("%s has no text content", name)
		}
		text := tr.Content[0].Text
		if strings.Contains(text, "dashboardLink") || strings.Contains(text, "_meta") {
			t.Errorf("%s text carries view meta: %s", name, text)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, tr.StructuredContent); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(compact.String(), "dashboardLink") || strings.Contains(compact.String(), "_meta") {
			t.Errorf("%s structuredContent carries view meta", name)
		}
		if text != compact.String() {
			t.Errorf("%s text is not the structuredContent JSON", name)
		}
	}
}

// TestGetRunViewRendersQualityLine asserts that the run card reads the
// quality summary through its one quality line.
func TestGetRunViewRendersQualityLine(t *testing.T) {
	src, err := uiFS.ReadFile("ui/get_run.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `["Quality", qualityLine(sc.quality)]`) {
		t.Fatal("get_run view does not render the quality line")
	}
}
