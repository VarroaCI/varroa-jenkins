package bundle

import (
	"strings"
	"testing"
)

func TestValidateContent_Valid(t *testing.T) {
	result := ValidateContent(
		"jenkins:\n  systemMessage: hello",
		"plugins:\n- artifactId: git\n  version: \"5.0\"",
		"items:\n- name: test-job",
		"roles:\n  admin:\n    permissions:\n    - Overall/Administer",
		map[string]string{"myvar": "val"},
	)
	if !result.Valid {
		t.Errorf("expected valid, got errors: %v", result.Errors)
	}
	if len(result.Errors) > 0 {
		t.Errorf("unexpected errors: %v", result.Errors)
	}
}

func TestValidateContent_UnresolvedVar(t *testing.T) {
	result := ValidateContent(
		"jenkins:\n  url: ${MY_VAR}\n",
		"",
		"",
		"",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid due to unresolved ${MY_VAR}")
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "${MY_VAR}") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error about ${MY_VAR}, got: %v", result.Errors)
	}
}

func TestValidateContent_EscapedVarAllowed(t *testing.T) {
	// ^${var} is a JCasC literal escape; Jenkins handles it, so the floor
	// must not flag it as unresolved.
	result := ValidateContent(
		"jenkins:\n  systemMessage: \"^${RUNTIME_SECRET}\"\n",
		"",
		"",
		"",
		nil,
	)
	if !result.Valid {
		t.Errorf("expected valid (^${} escape allowed), got errors: %v", result.Errors)
	}
}

func TestValidateContent_JCascSecretSourceAllowed(t *testing.T) {
	// ${readFile:...} / ${base64:...} are JCasC secret sources resolved by
	// Jenkins' own configuration-as-code interpolation, not Varroa's.
	result := ValidateContent(
		"credentials:\n  system:\n    domainCredentials:\n      - credentials:\n          - basicSSHUserPrivateKey:\n              privateKeySource:\n                directEntry:\n                  privateKey: ${readFile:/run/secrets/ssh-privatekey}\n",
		"",
		"",
		"",
		nil,
	)
	if !result.Valid {
		t.Errorf("expected valid (JCasC readFile secret source allowed), got errors: %v", result.Errors)
	}
}

func TestValidateContent_SecretSourceOutsideCascSectionsRejected(t *testing.T) {
	// JCasC interpolation only runs on casc-applied sections (jenkins.yaml,
	// rbac.yaml). A secret-source ref in items.yaml would reach Jenkins as a
	// literal string, so it must still fail validation.
	result := ValidateContent(
		"",
		"",
		"items:\n  - kind: pipeline\n    token: ${readFile:/run/secrets/ssh-privatekey}\n",
		"",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid: secret-source ref in items.yaml is not resolvable")
	}
}

func TestIsJCascSecretSourceRef_PrefixTable(t *testing.T) {
	// The prefix set mirrors casc's FixedInterpolatorStringLookup dispatch
	// table; commons-text lowercases the prefix before lookup, so matching
	// is case-insensitive.
	for _, name := range []string{
		"readFile:/run/secrets/key",
		"ReadFile:/run/secrets/key",
		"file:/etc/secret",
		"readFileBase64:/etc/cert.p12",
		"fileBase64:/etc/cert.p12",
		"base64:aGVsbG8=",
		"decodeBase64:aGVsbG8=",
		"json:key:{\"key\":\"v\"}",
		"sysProp:user.home",
		"trim: padded ",
	} {
		if !IsJCascSecretSourceRef(name) {
			t.Errorf("expected %q to be a JCasC secret-source ref", name)
		}
	}
	for _, name := range []string{
		"varroa_controller_name",
		"ssh-privatekey",
		"env:HOME",
		"readFileX:/nope",
		"readFile",
	} {
		if IsJCascSecretSourceRef(name) {
			t.Errorf("expected %q NOT to be a JCasC secret-source ref", name)
		}
	}
}

func TestValidateContent_VarroaVarAllowed(t *testing.T) {
	// varroa_* family is reserved and allowed unresolved at materialize time.
	result := ValidateContent(
		"jenkins:\n  url: ${varroa_controller_endpoint}\n  name: ${varroa_controller_name}\n",
		"",
		"",
		"",
		nil,
	)
	if !result.Valid {
		t.Errorf("expected valid (varroa_* vars allowed), got errors: %v", result.Errors)
	}
}

func TestValidateContent_DefinedVarAllowed(t *testing.T) {
	// A var defined in the bundle's variables is allowed unresolved.
	result := ValidateContent(
		"jenkins:\n  url: ${my_endpoint}\n",
		"",
		"",
		"",
		map[string]string{"my_endpoint": "https://example.com"},
	)
	if !result.Valid {
		t.Errorf("expected valid (defined var allowed), got errors: %v", result.Errors)
	}
}

func TestValidateContent_InvalidYAML(t *testing.T) {
	// An unclosed quote is truly invalid YAML.
	result := ValidateContent(
		"jenkins:\n  key: \"unclosed string\n",
		"",
		"",
		"",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid for unparseable YAML (unclosed quote)")
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "jcasc") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected error about jcasc YAML, got: %v", result.Errors)
	}
}

func TestValidateContent_InvalidPlugins(t *testing.T) {
	// Missing version is now an error, not a warning.
	result := ValidateContent(
		"",
		"plugins:\n- artifactId: git\n",
		"",
		"",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid for plugin without version")
	}
	hasVersionError := false
	for _, e := range result.Errors {
		if strings.Contains(e, "version is required") {
			hasVersionError = true
		}
	}
	if !hasVersionError {
		t.Errorf("expected error about version being required, got errors: %v", result.Errors)
	}

	// Version "latest" is a warning (still valid, but non-deterministic).
	result3 := ValidateContent(
		"",
		"plugins:\n- artifactId: git\n  version: \"latest\"\n",
		"",
		"",
		nil,
	)
	if !result3.Valid {
		t.Error("expected valid for plugin with version 'latest' (warning only)")
	}
	hasLatestWarning := false
	for _, w := range result3.Warnings {
		if strings.Contains(w, "latest") {
			hasLatestWarning = true
		}
	}
	if !hasLatestWarning {
		t.Errorf("expected warning about version 'latest', got warnings: %v", result3.Warnings)
	}

	// All plugins with exact versions: no error or warning from version check.
	result4 := ValidateContent(
		"",
		"plugins:\n- artifactId: git\n  version: \"5.0\"\n- artifactId: kubernetes\n  version: \"2.0\"\n",
		"",
		"",
		nil,
	)
	if !result4.Valid {
		t.Errorf("expected valid for plugins with exact versions, got errors: %v", result4.Errors)
	}
	for _, w := range result4.Warnings {
		if strings.Contains(w, "version") {
			t.Errorf("unexpected version-related warning for exact versions: %s", w)
		}
	}

	// Missing artifactId IS an error.
	result2 := ValidateContent(
		"",
		"plugins:\n- version: \"1.0\"\n",
		"",
		"",
		nil,
	)
	if result2.Valid {
		t.Error("expected invalid for plugin without artifactId")
	}
	hasError := false
	for _, e := range result2.Errors {
		if strings.Contains(e, "artifactId") {
			hasError = true
		}
	}
	if !hasError {
		t.Errorf("expected error about artifactId, got: %v", result2.Errors)
	}
}

func TestValidateContent_InvalidItemsYAML(t *testing.T) {
	result := ValidateContent(
		"",
		"",
		"not: valid: yaml: {{",
		"",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid for unparseable items YAML")
	}
}

func TestValidateContent_InvalidRbacYAML(t *testing.T) {
	result := ValidateContent(
		"",
		"",
		"",
		"{{ invalid",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid for unparseable RBAC YAML")
	}
}

func TestValidateContent_MixedUnresolvedAndVarroa(t *testing.T) {
	result := ValidateContent(
		"jenkins:\n  url: ${varroa_controller_endpoint}\n  key: ${UNDEFINED_VAR}\n",
		"",
		"",
		"",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid due to ${UNDEFINED_VAR}")
	}
	foundVarroa := false
	foundUndefined := false
	for _, e := range result.Errors {
		if strings.Contains(e, "${varroa_") {
			foundVarroa = true
		}
		if strings.Contains(e, "${UNDEFINED_VAR}") {
			foundUndefined = true
		}
	}
	if foundVarroa {
		t.Error("varroa_* should not be in errors")
	}
	if !foundUndefined {
		t.Error("expected error about ${UNDEFINED_VAR}")
	}
}

func TestValidateContent_EmptyContent(t *testing.T) {
	result := ValidateContent("", "", "", "", nil)
	if !result.Valid {
		t.Errorf("expected empty content to be valid, got errors: %v", result.Errors)
	}
}

func TestValidateContent_DuplicateVarInMultipleSections(t *testing.T) {
	result := ValidateContent(
		"jenkins:\n  key: ${MISSING}\n",
		"",
		"items:\n- name: ${MISSING}\n",
		"",
		nil,
	)
	if result.Valid {
		t.Error("expected invalid due to ${MISSING}")
	}
	// Should only report the var once, not per occurrence.
	count := 0
	for _, e := range result.Errors {
		if strings.Contains(e, "${MISSING}") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 error about ${MISSING}, got %d: %v", count, result.Errors)
	}
}

// validPipelineTemplateDoc is a well-formed PipelineTemplate document with
// an empty parameter schema, reused by the tests below.
const validPipelineTemplateDoc = `
apiVersion: varroa.dev/v1
kind: PipelineTemplate
metadata:
  name: go-service
spec:
  version: 1.0.0
  parameters:
    type: object
    additionalProperties: false
  jenkinsfile: "pipeline { stages { stage('build') { steps { echo 'hi' } } } } "
`

func TestValidateCatalogItem_Item_EmptyListValid(t *testing.T) {
	valid, msg := ValidateCatalogItem("item", []byte("items: []\n"), nil, "")
	if !valid || msg != "" {
		t.Errorf("expected valid with no message, got valid=%v msg=%q", valid, msg)
	}
}

func TestValidateCatalogItem_Item_NullListValid(t *testing.T) {
	valid, msg := ValidateCatalogItem("item", []byte("items:\n"), nil, "")
	if !valid || msg != "" {
		t.Errorf("expected valid with no message, got valid=%v msg=%q", valid, msg)
	}
}

func TestValidateCatalogItem_Item_NonEmptyListValid(t *testing.T) {
	valid, msg := ValidateCatalogItem("item", []byte("items:\n- kind: folder\n  name: a\n"), nil, "")
	if !valid || msg != "" {
		t.Errorf("expected valid with no message, got valid=%v msg=%q", valid, msg)
	}
}

func TestValidateCatalogItem_Item_MissingKeyRejected(t *testing.T) {
	for _, content := range []string{"", "jobs: []\n"} {
		valid, msg := ValidateCatalogItem("item", []byte(content), nil, "")
		if valid {
			t.Errorf("content %q: expected invalid", content)
		}
		if !strings.Contains(msg, "items key is missing") {
			t.Errorf("content %q: message = %q, want items key is missing", content, msg)
		}
	}
}

func TestValidateCatalogItem_Item_NonListRejected(t *testing.T) {
	valid, msg := ValidateCatalogItem("item", []byte("items: hello\n"), nil, "")
	if valid {
		t.Fatal("expected invalid for a non-list items value")
	}
	if msg == "" {
		t.Error("expected a non-empty message")
	}
}

func TestValidateCatalogItem_PipelineTemplate_ValidDocument(t *testing.T) {
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), nil, "")
	if !valid {
		t.Errorf("expected valid, got message: %s", msg)
	}
	if msg != "" {
		t.Errorf("expected no warnings, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_ItemYAMLContentRejected(t *testing.T) {
	// The MODIFIED "pipeline-template is a valid CatalogItemType" requirement's
	// "item-YAML content is now rejected" scenario: the old items:-list format
	// no longer validates.
	content := []byte(`
items:
  - kind: pipeline
    name: my-pipeline
    definition:
      script: "pipeline {}"
`)
	valid, msg := ValidateCatalogItem("pipeline-template", content, nil, "")
	if valid {
		t.Error("expected invalid for item-YAML content")
	}
	if !strings.Contains(msg, "PipelineTemplate document") {
		t.Errorf("expected message to state that content must be a PipelineTemplate document, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_NoBodyRejected(t *testing.T) {
	// The ADDED "pipeline-template content validates as a template document"
	// requirement's "a document with no body is rejected" scenario.
	content := []byte(`
apiVersion: varroa.dev/v1
kind: PipelineTemplate
metadata:
  name: go-service
spec:
  version: 1.0.0
  parameters:
    type: object
    additionalProperties: false
`)
	valid, msg := ValidateCatalogItem("pipeline-template", content, nil, "")
	if valid {
		t.Error("expected invalid for a document with neither jenkinsfile nor scmDefinition")
	}
	if !strings.Contains(msg, "jenkinsfile") || !strings.Contains(msg, "scmDefinition") {
		t.Errorf("expected message to name both keys, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_PreExistingUntypedVariableRemainsValid(t *testing.T) {
	// A pre-existing CatalogVarDecl with no Type set — the shape every
	// CatalogVariable in the repo/cluster had before this change.
	vars := []CatalogVarDecl{{Name: "x"}}
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), vars, "")
	if !valid {
		t.Errorf("expected untyped variable to remain valid, got message: %s", msg)
	}
	if !strings.Contains(msg, "x") {
		t.Errorf("expected a warning naming the variable, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_VariableWarnsItDoesNotReachTheBody(t *testing.T) {
	vars := []CatalogVarDecl{{Name: "goVersion"}}
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), vars, "")
	if !valid {
		t.Errorf("expected a declared variable to warn, not invalidate, got message: %s", msg)
	}
	if !strings.Contains(msg, "goVersion") {
		t.Errorf("expected the warning to name the variable, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_AllowedValuesOnBooleanRejected(t *testing.T) {
	vars := []CatalogVarDecl{{Name: "flag", Type: "boolean", AllowedValues: []string{"a", "b"}}}
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), vars, "")
	if valid {
		t.Error("expected invalid for allowedValues on a boolean variable")
	}
	if !strings.Contains(msg, "flag") {
		t.Errorf("expected message to identify the variable, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_AllowedValuesOnCredentialsRejected(t *testing.T) {
	vars := []CatalogVarDecl{{Name: "cred", Type: "credentials", AllowedValues: []string{"a", "b"}}}
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), vars, "")
	if valid {
		t.Error("expected invalid for allowedValues on a credentials variable")
	}
	if !strings.Contains(msg, "cred") {
		t.Errorf("expected message to identify the variable, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_AllowedValuesOnStringAccepted(t *testing.T) {
	vars := []CatalogVarDecl{{Name: "env", Type: "string", AllowedValues: []string{"dev", "prod"}}}
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), vars, "")
	if !valid {
		t.Errorf("expected allowedValues on a string variable to be valid, got message: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_UnknownTypeRejected(t *testing.T) {
	vars := []CatalogVarDecl{{Name: "weird", Type: "enum"}}
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), vars, "")
	if valid {
		t.Error("expected invalid for unknown variable type")
	}
	if !strings.Contains(msg, "weird") || !strings.Contains(msg, "enum") {
		t.Errorf("expected message to identify the variable and unknown type, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_VersionMismatchWarns(t *testing.T) {
	// The catalog-item-resolution delta's "A version mismatch warns" scenario:
	// the document's spec.version (1.0.0 here) wins, and both values are named.
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), nil, "2.0.0")
	if !valid {
		t.Errorf("expected a version mismatch to warn, not invalidate, got message: %s", msg)
	}
	if !strings.Contains(msg, "2.0.0") || !strings.Contains(msg, "1.0.0") {
		t.Errorf("expected the warning to name both versions, got: %s", msg)
	}
}

func TestValidateCatalogItem_PipelineTemplate_MatchingIndexVersionWarnsNothing(t *testing.T) {
	valid, msg := ValidateCatalogItem("pipeline-template", []byte(validPipelineTemplateDoc), nil, "1.0.0")
	if !valid {
		t.Errorf("expected valid, got message: %s", msg)
	}
	if msg != "" {
		t.Errorf("expected no warning when the index version matches the document, got: %s", msg)
	}
}

func TestIsVarroaVar(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"varroa_controller_name", true},
		{"varroa_controller_namespace", true},
		{"varroa_controller_endpoint", true},
		{"varroa_oidc_issuer", true},
		{"varroa_oidc_client_id", true},
		{"varroa_oidc_client_secret", true}, // still varroa_-prefixed even though it is not in InjectedVariableNames
		{"my_var", false},
		{"not_varroa", false},
		{"VARROA_controller", false}, // case-sensitive
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVarroaVar(tt.name); got != tt.want {
				t.Errorf("isVarroaVar(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestValidateContent_LatestVariantsWarn(t *testing.T) {
	for _, v := range []string{"LATEST", " latest "} {
		pluginsYAML := "plugins:\n- artifactId: git\n  version: \"" + v + "\"\n"
		result := ValidateContent("jenkins:\n  systemMessage: hi", pluginsYAML, "", "", nil)
		if !result.Valid {
			t.Errorf("version %q: expected valid (warning only), got errors: %v", v, result.Errors)
		}
		warned := false
		for _, w := range result.Warnings {
			if strings.Contains(w, "non-deterministic") {
				warned = true
			}
		}
		if !warned {
			t.Errorf("version %q: expected a non-deterministic warning, got %v", v, result.Warnings)
		}
	}
}

func TestValidateContent_BlankVersionIsError(t *testing.T) {
	result := ValidateContent("jenkins:\n  systemMessage: hi", "plugins:\n- artifactId: git\n  version: \"  \"\n", "", "", nil)
	if result.Valid {
		t.Fatal("a whitespace-only version must not validate")
	}
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "version is required") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a version-required error, got %v", result.Errors)
	}
}
