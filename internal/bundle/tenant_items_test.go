package bundle

import (
	"strings"
	"testing"
)

const tenantPollSCMItems = `items:
- kind: folder
  name: job
  items:
  - kind: freeStyle
    name: app
    triggers:
    - pollSCM: {}
`

const tenantCleanItems = `items:
- kind: freeStyle
  name: job
  builders:
  - shell:
      command: echo hi
`

func TestTenantItemsManifest_CleanItemsReRoot(t *testing.T) {
	m, err := TenantItemsManifest("payments", map[string]string{"items.yaml": tenantCleanItems})
	if err != nil {
		t.Fatalf("TenantItemsManifest: %v", err)
	}
	if len(m.Items) != 1 || m.Items[0].Name != "payments" || m.Items[0].Kind != "folder" {
		t.Fatalf("manifest = %+v, want one top-level folder payments", m.Items)
	}
}

func TestTenantItemsManifest_LintFindingFails(t *testing.T) {
	_, err := TenantItemsManifest("payments", map[string]string{"items.yaml": tenantPollSCMItems})
	if err == nil || !strings.Contains(err.Error(), "items lint failed") || !strings.Contains(err.Error(), "no-poll-scm") {
		t.Fatalf("TenantItemsManifest error = %v, want an items lint failed error naming no-poll-scm", err)
	}
}

func TestTenantItemsManifest_DroppedUnknownKeyFails(t *testing.T) {
	data := map[string]string{
		"items.yaml":                tenantCleanItems,
		DroppedItemKeysConfigMapKey: "- source: itemRef \"x\"\n  key: item\n",
	}
	_, err := TenantItemsManifest("payments", data)
	if err == nil || !strings.Contains(err.Error(), "drops unknown top-level keys") {
		t.Fatalf("TenantItemsManifest error = %v, want a drops unknown top-level keys error", err)
	}
}

func TestTenantItemsManifest_DroppedRootFails(t *testing.T) {
	data := map[string]string{
		"items.yaml":                tenantCleanItems,
		DroppedItemKeysConfigMapKey: "- source: itemRef \"x\"\n  key: root\n",
	}
	_, err := TenantItemsManifest("payments", data)
	if err == nil || !strings.Contains(err.Error(), `sets top-level key "root"`) {
		t.Fatalf("TenantItemsManifest error = %v, want a root error", err)
	}
}

func TestTenantItemsManifest_DroppedDeclaredKeyPasses(t *testing.T) {
	data := map[string]string{
		"items.yaml":                tenantCleanItems,
		DroppedItemKeysConfigMapKey: "- source: itemRef \"x\"\n  key: removeStrategy\n",
	}
	if _, err := TenantItemsManifest("payments", data); err != nil {
		t.Fatalf("TenantItemsManifest with a declared dropped key: %v", err)
	}
}

func TestTenantItemsManifest_UnparseableDroppedKeysFails(t *testing.T) {
	data := map[string]string{
		"items.yaml":                tenantCleanItems,
		DroppedItemKeysConfigMapKey: "not: [a list",
	}
	_, err := TenantItemsManifest("payments", data)
	if err == nil || !strings.Contains(err.Error(), DroppedItemKeysConfigMapKey) {
		t.Fatalf("TenantItemsManifest error = %v, want an error naming %s", err, DroppedItemKeysConfigMapKey)
	}
}

func TestLintTenantTemplates_FailingTemplateIsRemoved(t *testing.T) {
	fragment := `templates:
  - apiVersion: varroa.dev/v1
    kind: PipelineTemplate
    metadata:
      name: bad
    spec:
      version: 1.0.0
      parameters:
        type: object
        additionalProperties: false
      jenkinsfile: "node { jobDsl targets: 'jobs/*.groovy' }"
`
	rendered, findings := LintTenantTemplates(fragment)
	if len(findings) == 0 {
		t.Fatal("LintTenantTemplates returned no findings for a jobDsl template")
	}
	docs, err := ParsePipelineTemplates(rendered)
	if err != nil {
		t.Fatalf("ParsePipelineTemplates(rendered): %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("rendered fragment keeps %d templates, want the failing one removed", len(docs))
	}
}
