package bundle

import (
	"strings"
	"testing"
)

func TestRemainingVarRefs(t *testing.T) {
	vars := RemainingVarRefs(true,
		"jenkins:\n  url: ${MY_URL}\n  name: ${MY_NAME}\n",
		"",
		"items:\n- name: ${MY_ITEM}\n",
		"",
	)
	if len(vars) != 3 {
		t.Errorf("expected 3 unresolved vars, got %d: %v", len(vars), vars)
	}
	for _, want := range []string{"MY_URL", "MY_NAME", "MY_ITEM"} {
		found := false
		for _, v := range vars {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in unresolved vars", want)
		}
	}
}

func TestRemainingVarRefs_None(t *testing.T) {
	vars := RemainingVarRefs(true, "jenkins:\n  key: value\n", "", "", "")
	if len(vars) != 0 {
		t.Errorf("expected 0 unresolved vars, got %d: %v", len(vars), vars)
	}
}

func TestParseVariables_SkipsCommentsAndBlanks(t *testing.T) {
	vars := ParseVariables("# a comment\n\nalpha: one\n  beta:  two words \nnoColon\n")
	if len(vars) != 2 {
		t.Fatalf("ParseVariables kept %d entries, want 2: %v", len(vars), vars)
	}
	if vars["alpha"] != "one" || vars["beta"] != "two words" {
		t.Errorf("ParseVariables = %v, want alpha=one beta=two words", vars)
	}
}

func TestVarsReplacer_OnePass(t *testing.T) {
	r := VarsReplacer(Variables{"a": "${b}", "b": "bee"})
	got := r.Replace("x ${a} y ${b}")
	if got != "x ${b} y bee" {
		t.Errorf("VarsReplacer.Replace = %q, want %q (a value naming another variable stays unresolved)", got, "x ${b} y bee")
	}
}

func TestParseTenantItems_VarInKeyResolves(t *testing.T) {
	data := map[string]string{
		"variables.yaml": "field: builders\n",
		"items.yaml":     "items:\n  - kind: freeStyle\n    name: job\n    ${field}:\n      - shell:\n          command: echo hi\n",
	}
	m, err := ParseTenantItems(data)
	if err != nil {
		t.Fatalf("ParseTenantItems: %v", err)
	}
	if m == nil {
		t.Fatal("ParseTenantItems returned a nil manifest")
	}
}

func TestParseTenantItems_EmptyListGivesEmptyManifest(t *testing.T) {
	m, err := ParseTenantItems(map[string]string{"items.yaml": "items: []\n"})
	if err != nil {
		t.Fatalf("ParseTenantItems: %v", err)
	}
	if m == nil || len(m.Items) != 0 {
		t.Fatalf("manifest = %+v, want no items", m)
	}
}

func TestParseTenantItems_UnknownKeyFails(t *testing.T) {
	data := map[string]string{
		"items.yaml": "items:\n- name: job\n  notAKey: x\n",
	}
	if _, err := ParseTenantItems(data); err == nil || !strings.Contains(err.Error(), "parse tenant items.yaml") {
		t.Fatalf("ParseTenantItems error = %v, want a parse tenant items.yaml error", err)
	}
}

func TestParseTenantItems_UnresolvedVarFails(t *testing.T) {
	data := map[string]string{
		"items.yaml": "items:\n- name: ${missing}\n",
	}
	_, err := ParseTenantItems(data)
	if err == nil || !strings.Contains(err.Error(), "items.yaml holds unresolved variables: missing") {
		t.Fatalf("ParseTenantItems error = %v, want an unresolved variables error naming missing", err)
	}
}
