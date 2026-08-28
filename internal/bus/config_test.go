package bus

import (
	"encoding/json"
	"testing"
)

func TestOperatorBundlesSubject(t *testing.T) {
	tests := []struct {
		cluster string
		verb    string
		want    string
	}{
		{"my-cluster", "list", "operator.my-cluster.bundles.list"},
		{"my-cluster", "get", "operator.my-cluster.bundles.get"},
		{"my-cluster", "create", "operator.my-cluster.bundles.create"},
		{"my-cluster", "update", "operator.my-cluster.bundles.update"},
		{"my-cluster", "delete", "operator.my-cluster.bundles.delete"},
		{"my-cluster", "preview", "operator.my-cluster.bundles.preview"},
		{"my-cluster", "validate", "operator.my-cluster.bundles.validate"},
		{"my-cluster", "pause", "operator.my-cluster.bundles.pause"},
		{"my-cluster", "resume", "operator.my-cluster.bundles.resume"},
	}
	for _, tt := range tests {
		got := OperatorBundlesSubject(tt.cluster, tt.verb)
		if got != tt.want {
			t.Errorf("OperatorBundlesSubject(%q, %q) = %q, want %q", tt.cluster, tt.verb, got, tt.want)
		}
	}
}

func TestOperatorCatalogSubject(t *testing.T) {
	tests := []struct {
		cluster string
		verb    string
		want    string
	}{
		{"my-cluster", "itemlist", "operator.my-cluster.catalog.itemlist"},
		{"my-cluster", "itemget", "operator.my-cluster.catalog.itemget"},
		{"my-cluster", "sourcelist", "operator.my-cluster.catalog.sourcelist"},
		{"my-cluster", "sourceget", "operator.my-cluster.catalog.sourceget"},
		{"my-cluster", "sourcecreate", "operator.my-cluster.catalog.sourcecreate"},
		{"my-cluster", "sourceupdate", "operator.my-cluster.catalog.sourceupdate"},
		{"my-cluster", "sourcedelete", "operator.my-cluster.catalog.sourcedelete"},
		{"my-cluster", "sourcesync", "operator.my-cluster.catalog.sourcesync"},
	}
	for _, tt := range tests {
		got := OperatorCatalogSubject(tt.cluster, tt.verb)
		if got != tt.want {
			t.Errorf("OperatorCatalogSubject(%q, %q) = %q, want %q", tt.cluster, tt.verb, got, tt.want)
		}
	}
}

func TestOperatorRbacSubject(t *testing.T) {
	tests := []struct {
		cluster string
		verb    string
		want    string
	}{
		{"my-cluster", "rolelist", "operator.my-cluster.rbac.rolelist"},
		{"my-cluster", "roleget", "operator.my-cluster.rbac.roleget"},
		{"my-cluster", "rolecreate", "operator.my-cluster.rbac.rolecreate"},
		{"my-cluster", "roleupdate", "operator.my-cluster.rbac.roleupdate"},
		{"my-cluster", "roledelete", "operator.my-cluster.rbac.roledelete"},
		{"my-cluster", "bindinglist", "operator.my-cluster.rbac.bindinglist"},
		{"my-cluster", "bindingget", "operator.my-cluster.rbac.bindingget"},
		{"my-cluster", "bindingcreate", "operator.my-cluster.rbac.bindingcreate"},
		{"my-cluster", "bindingupdate", "operator.my-cluster.rbac.bindingupdate"},
		{"my-cluster", "bindingdelete", "operator.my-cluster.rbac.bindingdelete"},
	}
	for _, tt := range tests {
		got := OperatorRbacSubject(tt.cluster, tt.verb)
		if got != tt.want {
			t.Errorf("OperatorRbacSubject(%q, %q) = %q, want %q", tt.cluster, tt.verb, got, tt.want)
		}
	}
}

// TestAll27Subjects verifies that the union of all three subject builders
// produces exactly 27 subjects for a sample cluster.
func TestAll27Subjects(t *testing.T) {
	cluster := "dev-cluster"
	got := make(map[string]bool)

	bundleVerbs := []string{"list", "get", "create", "update", "delete", "preview", "validate", "pause", "resume"}
	for _, v := range bundleVerbs {
		got[OperatorBundlesSubject(cluster, v)] = true
	}

	catalogVerbs := []string{"itemlist", "itemget", "sourcelist", "sourceget", "sourcecreate", "sourceupdate", "sourcedelete", "sourcesync"}
	for _, v := range catalogVerbs {
		got[OperatorCatalogSubject(cluster, v)] = true
	}

	rbacVerbs := []string{"rolelist", "roleget", "rolecreate", "roleupdate", "roledelete", "bindinglist", "bindingget", "bindingcreate", "bindingupdate", "bindingdelete"}
	for _, v := range rbacVerbs {
		got[OperatorRbacSubject(cluster, v)] = true
	}

	if len(got) != 27 {
		t.Errorf("expected 27 unique subjects, got %d", len(got))
	}

	want := map[string]bool{
		"operator.dev-cluster.bundles.list":         true,
		"operator.dev-cluster.bundles.get":          true,
		"operator.dev-cluster.bundles.create":       true,
		"operator.dev-cluster.bundles.update":       true,
		"operator.dev-cluster.bundles.delete":       true,
		"operator.dev-cluster.bundles.preview":      true,
		"operator.dev-cluster.bundles.validate":     true,
		"operator.dev-cluster.bundles.pause":        true,
		"operator.dev-cluster.bundles.resume":       true,
		"operator.dev-cluster.catalog.itemlist":     true,
		"operator.dev-cluster.catalog.itemget":      true,
		"operator.dev-cluster.catalog.sourcelist":   true,
		"operator.dev-cluster.catalog.sourceget":    true,
		"operator.dev-cluster.catalog.sourcecreate": true,
		"operator.dev-cluster.catalog.sourceupdate": true,
		"operator.dev-cluster.catalog.sourcedelete": true,
		"operator.dev-cluster.catalog.sourcesync":   true,
		"operator.dev-cluster.rbac.rolelist":        true,
		"operator.dev-cluster.rbac.roleget":         true,
		"operator.dev-cluster.rbac.rolecreate":      true,
		"operator.dev-cluster.rbac.roleupdate":      true,
		"operator.dev-cluster.rbac.roledelete":      true,
		"operator.dev-cluster.rbac.bindinglist":     true,
		"operator.dev-cluster.rbac.bindingget":      true,
		"operator.dev-cluster.rbac.bindingcreate":   true,
		"operator.dev-cluster.rbac.bindingupdate":   true,
		"operator.dev-cluster.rbac.bindingdelete":   true,
	}

	for s := range got {
		if !want[s] {
			t.Errorf("unexpected subject %q", s)
		}
	}
	for s := range want {
		if !got[s] {
			t.Errorf("missing subject %q", s)
		}
	}
}

func TestConfigPayloadRoundTrips(t *testing.T) {
	// ConfigListRequest
	req := ConfigListRequest{Namespace: "ns-a"}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got ConfigListRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Namespace != "ns-a" {
		t.Errorf("ConfigListRequest round-trip: got Namespace=%q", got.Namespace)
	}

	// ConfigListResponse with operator namespace
	resp := ConfigListResponse{
		Items:             []json.RawMessage{json.RawMessage(`{"a":1}`)},
		OperatorNamespace: "op-ns",
		Code:              CodeNotFound,
		Error:             "not found",
	}
	data, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var gotResp ConfigListResponse
	if err := json.Unmarshal(data, &gotResp); err != nil {
		t.Fatal(err)
	}
	if len(gotResp.Items) != 1 {
		t.Errorf("ConfigListResponse.Items: got %d, want 1", len(gotResp.Items))
	}
	if gotResp.OperatorNamespace != "op-ns" {
		t.Errorf("ConfigListResponse.OperatorNamespace: got %q", gotResp.OperatorNamespace)
	}
	if gotResp.Code != CodeNotFound {
		t.Errorf("ConfigListResponse.Code: got %q", gotResp.Code)
	}

	// ConfigGetRequest
	getReq := ConfigGetRequest{Namespace: "ns-a", Name: "my-cr"}
	data, _ = json.Marshal(getReq)
	var gotGet ConfigGetRequest
	if err := json.Unmarshal(data, &gotGet); err != nil {
		t.Fatal(err)
	}
	if gotGet.Namespace != "ns-a" || gotGet.Name != "my-cr" {
		t.Errorf("ConfigGetRequest round-trip: got %+v", gotGet)
	}

	// ConfigGetResponse
	getResp := ConfigGetResponse{Item: json.RawMessage(`{"b":2}`), Code: CodeConflict}
	data, _ = json.Marshal(getResp)
	var gotGetResp ConfigGetResponse
	if err := json.Unmarshal(data, &gotGetResp); err != nil {
		t.Fatal(err)
	}
	if string(gotGetResp.Item) != `{"b":2}` {
		t.Errorf("ConfigGetResponse.Item: got %s", string(gotGetResp.Item))
	}

	// ConfigApplyRequest
	appReq := ConfigApplyRequest{Namespace: "ns-a", Name: "my-cr", Object: json.RawMessage(`{"spec":{}}`)}
	data, _ = json.Marshal(appReq)
	var gotApp ConfigApplyRequest
	if err := json.Unmarshal(data, &gotApp); err != nil {
		t.Fatal(err)
	}
	if gotApp.Namespace != "ns-a" || gotApp.Name != "my-cr" || string(gotApp.Object) != `{"spec":{}}` {
		t.Errorf("ConfigApplyRequest round-trip: got %+v", gotApp)
	}

	// ConfigApplyResponse
	appResp := ConfigApplyResponse{Item: json.RawMessage(`{"c":3}`), Code: CodeInvalid}
	data, _ = json.Marshal(appResp)
	var gotAppResp ConfigApplyResponse
	if err := json.Unmarshal(data, &gotAppResp); err != nil {
		t.Fatal(err)
	}
	if string(gotAppResp.Item) != `{"c":3}` {
		t.Errorf("ConfigApplyResponse.Item: got %s", string(gotAppResp.Item))
	}

	// ConfigDeleteRequest
	delReq := ConfigDeleteRequest{Namespace: "ns-a", Name: "my-cr"}
	data, _ = json.Marshal(delReq)
	var gotDel ConfigDeleteRequest
	if err := json.Unmarshal(data, &gotDel); err != nil {
		t.Fatal(err)
	}
	if gotDel.Namespace != "ns-a" || gotDel.Name != "my-cr" {
		t.Errorf("ConfigDeleteRequest round-trip: got %+v", gotDel)
	}

	// ConfigDeleteResponse
	delResp := ConfigDeleteResponse{Code: CodeInternal, Error: "oops"}
	data, _ = json.Marshal(delResp)
	var gotDelResp ConfigDeleteResponse
	if err := json.Unmarshal(data, &gotDelResp); err != nil {
		t.Fatal(err)
	}
	if gotDelResp.Code != CodeInternal || gotDelResp.Error != "oops" {
		t.Errorf("ConfigDeleteResponse round-trip: got %+v", gotDelResp)
	}

	// BundlePauseRequest
	pReq := BundlePauseRequest{Namespace: "ns-a", Name: "bundle1", Paused: true}
	data, _ = json.Marshal(pReq)
	var gotPause BundlePauseRequest
	if err := json.Unmarshal(data, &gotPause); err != nil {
		t.Fatal(err)
	}
	if gotPause.Namespace != "ns-a" || gotPause.Name != "bundle1" || !gotPause.Paused {
		t.Errorf("BundlePauseRequest round-trip: got %+v", gotPause)
	}

	// BundlePauseResponse
	pResp := BundlePauseResponse{Code: CodeNotFound, Error: "bundle not found"}
	data, _ = json.Marshal(pResp)
	var gotPauseResp BundlePauseResponse
	if err := json.Unmarshal(data, &gotPauseResp); err != nil {
		t.Fatal(err)
	}
	if gotPauseResp.Code != CodeNotFound {
		t.Errorf("BundlePauseResponse.Code: got %q", gotPauseResp.Code)
	}

	// BundleComposeRequest
	compReq := BundleComposeRequest{Namespace: "ns-a", Spec: json.RawMessage(`{"gitSource":{}}`)}
	data, _ = json.Marshal(compReq)
	var gotComp BundleComposeRequest
	if err := json.Unmarshal(data, &gotComp); err != nil {
		t.Fatal(err)
	}
	if gotComp.Namespace != "ns-a" || string(gotComp.Spec) != `{"gitSource":{}}` {
		t.Errorf("BundleComposeRequest round-trip: got %+v", gotComp)
	}

	// BundleComposeResponse with preview
	preview := &BundleComposePreview{
		BundleYAML:          "bundle: yaml",
		JenkinsYAML:         "jenkins: yaml",
		PluginsYAML:         "plugins: yaml",
		ItemsYAML:           "items: yaml",
		RbacYAML:            "rbac: yaml",
		Missing:             []string{"item-a"},
		Drifted:             []string{"item-b"},
		Warnings:            []string{"warn1"},
		UnresolvedVariables: []string{"${VAR}"},
		Errors:              []string{"err1"},
	}
	compResp := BundleComposeResponse{Preview: preview, Code: CodeInvalid, Error: "validation error"}
	data, _ = json.Marshal(compResp)
	var gotCompResp BundleComposeResponse
	if err := json.Unmarshal(data, &gotCompResp); err != nil {
		t.Fatal(err)
	}
	if gotCompResp.Preview == nil {
		t.Fatal("BundleComposeResponse.Preview is nil after round-trip")
	}
	if gotCompResp.Preview.BundleYAML != "bundle: yaml" {
		t.Errorf("BundleComposePreview.BundleYAML: got %q", gotCompResp.Preview.BundleYAML)
	}
	if len(gotCompResp.Preview.UnresolvedVariables) != 1 || gotCompResp.Preview.UnresolvedVariables[0] != "${VAR}" {
		t.Errorf("BundleComposePreview.UnresolvedVariables: got %v", gotCompResp.Preview.UnresolvedVariables)
	}
	if gotCompResp.Code != CodeInvalid {
		t.Errorf("BundleComposeResponse.Code: got %q", gotCompResp.Code)
	}

	// CatalogSyncRequest
	syncReq := CatalogSyncRequest{Namespace: "ns-a", Name: "src1"}
	data, _ = json.Marshal(syncReq)
	var gotSync CatalogSyncRequest
	if err := json.Unmarshal(data, &gotSync); err != nil {
		t.Fatal(err)
	}
	if gotSync.Namespace != "ns-a" || gotSync.Name != "src1" {
		t.Errorf("CatalogSyncRequest round-trip: got %+v", gotSync)
	}

	// CatalogSyncResponse
	syncResp := CatalogSyncResponse{Code: CodeInternal, Error: "sync failed"}
	data, _ = json.Marshal(syncResp)
	var gotSyncResp CatalogSyncResponse
	if err := json.Unmarshal(data, &gotSyncResp); err != nil {
		t.Fatal(err)
	}
	if gotSyncResp.Code != CodeInternal {
		t.Errorf("CatalogSyncResponse.Code: got %q", gotSyncResp.Code)
	}
}
