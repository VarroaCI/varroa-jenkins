package bundle

import (
	"testing"

	"gopkg.in/yaml.v3"
)

const updateSiteTestURL = "http://uc.varroa-system.svc:8080/update-center.json"

func parseUpdateSites(t *testing.T, doc string) []interface{} {
	t.Helper()
	got := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(doc), &got); err != nil {
		t.Fatalf("parse jenkins yaml: %v", err)
	}
	jenkins, ok := got["jenkins"].(map[string]interface{})
	if !ok {
		t.Fatalf("jenkins block missing in %q", doc)
	}
	uc, ok := jenkins["updateCenter"].(map[string]interface{})
	if !ok {
		t.Fatalf("jenkins.updateCenter missing in %q", doc)
	}
	sites, ok := uc["sites"].([]interface{})
	if !ok {
		t.Fatalf("jenkins.updateCenter.sites is not a list in %q", doc)
	}
	return sites
}

func TestInjectUpdateSites_AddsWhenAbsent(t *testing.T) {
	out, overrode, err := InjectUpdateSites("jenkins:\n  systemMessage: hi\n", updateSiteTestURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrode {
		t.Error("expected overrode=false when no updateCenter exists")
	}
	sites := parseUpdateSites(t, out)
	if len(sites) != 1 {
		t.Fatalf("sites = %v, want one entry", sites)
	}
	site := sites[0].(map[string]interface{})
	if site["id"] != "default" || site["url"] != updateSiteTestURL {
		t.Errorf("site = %v, want id default and url %q", site, updateSiteTestURL)
	}
}

func TestInjectUpdateSites_ReplacesBundleSites(t *testing.T) {
	in := "jenkins:\n  updateCenter:\n    sites:\n      - id: bundle\n        url: https://example.com/uc.json\n"
	out, overrode, err := InjectUpdateSites(in, updateSiteTestURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !overrode {
		t.Error("expected overrode=true when the bundle set a different site")
	}
	sites := parseUpdateSites(t, out)
	if len(sites) != 1 {
		t.Fatalf("sites = %v, want exactly the operator site", sites)
	}
	if site := sites[0].(map[string]interface{}); site["id"] != "default" {
		t.Errorf("site id = %v, want default", site["id"])
	}
}

func TestInjectUpdateSites_SameValueNotOverrode(t *testing.T) {
	first, _, err := InjectUpdateSites("", updateSiteTestURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, overrode, err := InjectUpdateSites(first, updateSiteTestURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrode {
		t.Error("expected overrode=false when the injected value is unchanged")
	}
}

func TestInjectUpdateSites_EmptyURLWritesEmptyList(t *testing.T) {
	out, _, err := InjectUpdateSites("jenkins:\n  systemMessage: hi\n", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sites := parseUpdateSites(t, out)
	if len(sites) != 0 {
		t.Errorf("sites = %v, want empty list", sites)
	}
}

func TestInjectUpdateSites_EmptyDocument(t *testing.T) {
	out, overrode, err := InjectUpdateSites("", updateSiteTestURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overrode {
		t.Error("expected overrode=false for empty input")
	}
	if sites := parseUpdateSites(t, out); len(sites) != 1 {
		t.Errorf("sites = %v, want one entry", sites)
	}
}

func TestInjectUpdateSites_KeepsOtherJenkinsKeys(t *testing.T) {
	in := "jenkins:\n  systemMessage: hi\n  numExecutors: 2\nunclassified:\n  location:\n    url: https://x/\n"
	out, _, err := InjectUpdateSites(in, updateSiteTestURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := map[string]interface{}{}
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	jenkins := got["jenkins"].(map[string]interface{})
	if jenkins["systemMessage"] != "hi" || jenkins["numExecutors"] != 2 {
		t.Errorf("jenkins keys lost: %v", jenkins)
	}
	if _, ok := got["unclassified"].(map[string]interface{}); !ok {
		t.Errorf("unclassified block lost: %v", got)
	}
}
