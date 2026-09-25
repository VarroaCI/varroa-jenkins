package controller

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
)

// golangPodTemplate is the golang template of the platform catalog's
// pod-templates/golang.yaml item. The catalog is a separate repository, so
// the test keeps its own copy.
const golangPodTemplate = `- name: golang
  label: golang
  idleMinutes: 5
  podRetention: "never"
  containers:
    - name: golang
      image: golang:1.26-bookworm
      command: sleep
      args: 99d
      ttyEnabled: true
      workingDir: /home/jenkins/agent
      resourceRequestCpu: "2"
      resourceRequestMemory: "4Gi"
      resourceLimitCpu: "4"
      resourceLimitMemory: "8Gi"
  volumes:
    - emptyDirVolume:
        mountPath: /go/pkg/mod
    - emptyDirVolume:
        mountPath: /root/.cache/go-build
`

// minimalCloud wraps a YAML list of pod templates in the composer's
// minimal kubernetes cloud.
func minimalCloud(templates string) string {
	var list []any
	if err := yaml.Unmarshal([]byte(templates), &list); err != nil {
		panic(err)
	}
	out, err := yaml.Marshal(map[string]any{
		"jenkins": map[string]any{
			"clouds": []any{map[string]any{"kubernetes": map[string]any{"templates": list}}},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(out)
}

func liftedByName(lift tenantPodTemplateLift, name string) *bundle.TenantCloudTemplate {
	for i := range lift.Templates {
		if lift.Templates[i].Name == name {
			return &lift.Templates[i]
		}
	}
	return nil
}

func warningMessages(ws []v1alpha1.TenantWarning, reason string) []string {
	var out []string
	for _, w := range ws {
		if w.Reason == reason {
			out = append(out, w.Message)
		}
	}
	return out
}

func TestLiftTenantPodTemplates_GolangCatalogItem(t *testing.T) {
	lift := liftTenantPodTemplates("payments", "payments-ns", minimalCloud(golangPodTemplate), nil)
	if len(lift.Warnings) != 0 {
		t.Fatalf("expected no warning, got %+v", lift.Warnings)
	}
	if len(lift.Templates) != 1 {
		t.Fatalf("expected one lifted template, got %d", len(lift.Templates))
	}
	got := lift.Templates[0]
	if got.Name != "payments-golang" || got.Namespace != "payments-ns" || got.ServiceAccount != "varroa-agent-payments" {
		t.Errorf("unexpected stamps: %+v", got)
	}
	if got.Labels != "varroa-tenant-payments payments-golang" {
		t.Errorf("unexpected label %q", got.Labels)
	}
	if len(got.Containers) != 0 {
		t.Errorf("expected no typed containers, got %+v", got.Containers)
	}

	var fixture []map[string]any
	if err := yaml.Unmarshal([]byte(golangPodTemplate), &fixture); err != nil {
		t.Fatal(err)
	}
	want := fixture[0]
	delete(want, "name")
	delete(want, "label")
	if !reflect.DeepEqual(got.Raw, want) {
		t.Errorf("pass-through keys differ:\n got %#v\nwant %#v", got.Raw, want)
	}
}

func TestLiftTenantPodTemplates_Stamps(t *testing.T) {
	cases := []struct {
		name       string
		template   string
		vars       bundle.Variables
		wantLabel  string
		wantRaw    map[string]any
		wantAbsent []string
	}{
		{
			name:      "label tokens keep their order",
			template:  "- name: golang\n  label: golang linux\n",
			wantLabel: "varroa-tenant-payments payments-golang payments-linux",
			wantRaw:   map[string]any{},
		},
		{
			name:      "no label gets the reserved token only",
			template:  "- name: golang\n",
			wantLabel: "varroa-tenant-payments",
			wantRaw:   map[string]any{},
		},
		{
			name:      "a forged reserved token is dropped",
			template:  "- name: golang\n  label: varroa-tenant-search linux\n",
			wantLabel: "varroa-tenant-payments payments-linux",
			wantRaw:   map[string]any{},
		},
		{
			name:       "a labels key is dropped",
			template:   "- name: golang\n  label: golang\n  labels: golang\n",
			wantLabel:  "varroa-tenant-payments payments-golang",
			wantRaw:    map[string]any{},
			wantAbsent: []string{"labels"},
		},
		{
			name:       "tenant namespace and serviceAccount are dropped",
			template:   "- name: golang\n  namespace: kube-system\n  serviceAccount: admin\n",
			wantLabel:  "varroa-tenant-payments",
			wantRaw:    map[string]any{},
			wantAbsent: []string{"namespace", "serviceAccount", "name", "label"},
		},
		{
			name:      "a tenant variable resolves",
			template:  "- name: golang\n  containers:\n    - name: golang\n      image: ${golang_image}\n",
			vars:      bundle.Variables{"golang_image": "golang:1.26-bookworm"},
			wantLabel: "varroa-tenant-payments",
			wantRaw: map[string]any{"containers": []any{map[string]any{
				"name": "golang", "image": "golang:1.26-bookworm",
			}}},
		},
		{
			name:      "a hostPath yaml passes through",
			template:  "- name: golang\n  yaml: |\n    spec:\n      volumes:\n        - name: docker\n          hostPath:\n            path: /var/run/docker.sock\n",
			wantLabel: "varroa-tenant-payments",
			wantRaw:   map[string]any{"yaml": "spec:\n  volumes:\n    - name: docker\n      hostPath:\n        path: /var/run/docker.sock\n"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lift := liftTenantPodTemplates("payments", "payments-ns", minimalCloud(tc.template), tc.vars)
			if len(lift.Warnings) != 0 {
				t.Fatalf("expected no warning, got %+v", lift.Warnings)
			}
			got := liftedByName(lift, "payments-golang")
			if got == nil {
				t.Fatalf("expected payments-golang, got %+v", lift.Templates)
			}
			if got.Namespace != "payments-ns" || got.ServiceAccount != "varroa-agent-payments" {
				t.Errorf("unexpected stamps: %+v", got)
			}
			if got.Labels != tc.wantLabel {
				t.Errorf("label = %q, want %q", got.Labels, tc.wantLabel)
			}
			if !reflect.DeepEqual(got.Raw, tc.wantRaw) {
				t.Errorf("Raw = %#v, want %#v", got.Raw, tc.wantRaw)
			}
			for _, k := range tc.wantAbsent {
				if _, ok := got.Raw[k]; ok {
					t.Errorf("expected %q absent from Raw", k)
				}
			}
		})
	}
}

func TestLiftTenantPodTemplates_InheritFrom(t *testing.T) {
	doc := minimalCloud("- name: base\n- name: golang\n  inheritFrom: base other\n- name: odd\n  inheritFrom: [base]\n")
	lift := liftTenantPodTemplates("payments", "payments-ns", doc, nil)
	if len(lift.Warnings) != 0 {
		t.Fatalf("expected no warning, got %+v", lift.Warnings)
	}
	if got := liftedByName(lift, "payments-golang").Raw["inheritFrom"]; got != "payments-base other" {
		t.Errorf("inheritFrom = %v, want %q", got, "payments-base other")
	}
	if got := liftedByName(lift, "payments-odd").Raw["inheritFrom"]; !reflect.DeepEqual(got, []any{"base"}) {
		t.Errorf("a non-string inheritFrom changed: %#v", got)
	}
}

func TestLiftTenantPodTemplates_SkipRules(t *testing.T) {
	cases := []struct {
		name      string
		doc       string
		vars      bundle.Variables
		wantKept  []string
		wantSkips []string
	}{
		{
			name:      "an entry that is not a map",
			doc:       minimalCloud("- name: golang\n- just-a-string\n"),
			wantKept:  []string{"payments-golang"},
			wantSkips: []string{"jenkins.clouds[0].kubernetes.templates[1]: entry is not a map"},
		},
		{
			name:      "a missing name",
			doc:       minimalCloud("- label: golang\n- name: \"\"\n- name: golang\n"),
			wantKept:  []string{"payments-golang"},
			wantSkips: []string{"jenkins.clouds[0].kubernetes.templates[0]: name is missing or empty", "jenkins.clouds[0].kubernetes.templates[1]: name is missing or empty"},
		},
		{
			name:      "a name that is not a string",
			doc:       minimalCloud("- name: [golang]\n"),
			wantSkips: []string{"jenkins.clouds[0].kubernetes.templates[0]: name is not a string"},
		},
		{
			name:      "the reserved name default",
			doc:       minimalCloud("- name: default\n- name: golang\n"),
			wantKept:  []string{"payments-golang"},
			wantSkips: []string{`pod template "default": the name "default" is reserved`},
		},
		{
			name:      "a label that is not a string",
			doc:       minimalCloud("- name: golang\n  label: [golang]\n"),
			wantSkips: []string{`pod template "golang": label is not a string`},
		},
		{
			name:      "a null label",
			doc:       minimalCloud("- name: golang\n  label: null\n"),
			wantSkips: []string{`pod template "golang": label is not a string`},
		},
		{
			name:      "a duplicate name in the same cloud",
			doc:       minimalCloud("- name: golang\n  idleMinutes: 1\n- name: golang\n  idleMinutes: 2\n"),
			wantKept:  []string{"payments-golang"},
			wantSkips: []string{`pod template "golang": an earlier template has the same name`},
		},
		{
			name:      "an unknown variable",
			doc:       minimalCloud("- name: golang\n  yaml: ${x}\n"),
			wantSkips: []string{`pod template "golang": a value or key holds "${" after the tenant's variables resolve`},
		},
		{
			name:      "a readFile secret source",
			doc:       minimalCloud("- name: golang\n  yaml: ${readFile:/x}\n"),
			wantSkips: []string{`pod template "golang": a value or key holds "${" after the tenant's variables resolve`},
		},
		{
			name:      "a cell variable",
			doc:       minimalCloud("- name: golang\n  yaml: ${cellVar}\n"),
			wantSkips: []string{`pod template "golang": a value or key holds "${" after the tenant's variables resolve`},
		},
		{
			name:      "an escaped reference",
			doc:       minimalCloud("- name: golang\n  yaml: ^${x}\n"),
			vars:      bundle.Variables{"y": "z"},
			wantSkips: []string{`pod template "golang": a value or key holds "${" after the tenant's variables resolve`},
		},
		{
			name:      "a reference in a map key",
			doc:       minimalCloud("- name: golang\n  annotations:\n    ${x}: v\n"),
			wantSkips: []string{`pod template "golang": a value or key holds "${" after the tenant's variables resolve`},
		},
		{
			name:      "a reference in the name resolves before the name rules",
			doc:       minimalCloud("- name: ${tool}\n"),
			vars:      bundle.Variables{"tool": "default"},
			wantSkips: []string{`pod template "default": the name "default" is reserved`},
		},
		{
			name: "bundle order across two clouds, the duplicate in the second cloud",
			doc: "jenkins:\n  clouds:\n" +
				"    - kubernetes:\n        templates:\n          - name: base\n          - name: golang\n" +
				"    - kubernetes:\n        templates:\n          - name: golang\n          - name: node\n",
			wantKept:  []string{"payments-base", "payments-golang", "payments-node"},
			wantSkips: []string{`pod template "golang": an earlier template has the same name`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lift := liftTenantPodTemplates("payments", "payments-ns", tc.doc, tc.vars)
			var kept []string
			for _, tpl := range lift.Templates {
				kept = append(kept, tpl.Name)
			}
			if !reflect.DeepEqual(kept, tc.wantKept) {
				t.Errorf("kept = %v, want %v", kept, tc.wantKept)
			}
			if got := warningMessages(lift.Warnings, v1alpha1.TeamReasonPodTemplateSkipped); !reflect.DeepEqual(got, tc.wantSkips) {
				t.Errorf("skips = %q, want %q", got, tc.wantSkips)
			}
			for _, w := range lift.Warnings {
				if w.Tenant != "payments" {
					t.Errorf("warning names tenant %q, want payments", w.Tenant)
				}
				if w.Reason != v1alpha1.TeamReasonPodTemplateSkipped {
					t.Errorf("unexpected warning %+v", w)
				}
			}
		})
	}
}

// TestLiftTenantPodTemplates_ChainedVariableAlwaysSkips proves that the
// resolve pass is one deterministic pass: a variable whose value names
// another variable keeps its "${" on every run, so the template is skipped
// on every tick rather than on some ticks only.
func TestLiftTenantPodTemplates_ChainedVariableAlwaysSkips(t *testing.T) {
	doc := minimalCloud("- name: golang\n  yaml: ${a}\n")
	vars := bundle.Variables{"a": "${b}", "b": "x", "c": "y", "d": "z", "e": "w"}
	for i := 0; i < 50; i++ {
		lift := liftTenantPodTemplates("payments", "payments-ns", doc, vars)
		if len(lift.Templates) != 0 {
			t.Fatalf("run %d: expected the chained reference skipped, got %+v", i, lift.Templates)
		}
	}
}

func TestLiftTenantPodTemplates_DuplicateKeepsFirst(t *testing.T) {
	lift := liftTenantPodTemplates("payments", "payments-ns",
		minimalCloud("- name: golang\n  idleMinutes: 1\n- name: golang\n  idleMinutes: 2\n"), nil)
	if got := liftedByName(lift, "payments-golang").Raw["idleMinutes"]; got != 1 {
		t.Errorf("expected the first template kept, got idleMinutes %v", got)
	}
}

func TestLiftTenantPodTemplates_Residue(t *testing.T) {
	const prefix = `tenant bundle key "jenkins.yaml" is ignored on a cell except its pod templates: `
	cases := []struct {
		name      string
		doc       string
		wantKept  int
		wantWarns []string
	}{
		{name: "a blank document", doc: "  \n"},
		{name: "pod templates only", doc: minimalCloud(golangPodTemplate), wantKept: 1},
		{
			name:      "jenkins.systemMessage",
			doc:       "jenkins:\n  systemMessage: hi\n  clouds:\n    - kubernetes:\n        templates:\n          - name: golang\n",
			wantKept:  1,
			wantWarns: []string{prefix + "jenkins.systemMessage"},
		},
		{
			name:      "a top-level unclassified key",
			doc:       "unclassified:\n  location:\n    url: http://x/\n",
			wantWarns: []string{prefix + "unclassified"},
		},
		{
			name:      "jenkins is not a map",
			doc:       "jenkins: hello\n",
			wantWarns: []string{prefix + "jenkins"},
		},
		{
			name:      "jenkins.clouds is not a list",
			doc:       "jenkins:\n  clouds: hello\n",
			wantWarns: []string{prefix + "jenkins.clouds"},
		},
		{
			name:      "a docker cloud entry",
			doc:       "jenkins:\n  clouds:\n    - docker:\n        name: d\n    - kubernetes:\n        templates:\n          - name: golang\n",
			wantKept:  1,
			wantWarns: []string{prefix + "jenkins.clouds[0]"},
		},
		{
			name:      "a key beside kubernetes",
			doc:       "jenkins:\n  clouds:\n    - kubernetes:\n        templates:\n          - name: golang\n      extra: 1\n",
			wantKept:  1,
			wantWarns: []string{prefix + "jenkins.clouds[0].extra"},
		},
		{
			name:      "unclassified and a serverUrl beside templates",
			doc:       "unclassified: {}\njenkins:\n  clouds:\n    - kubernetes:\n        serverUrl: https://k8s\n        templates:\n          - name: golang\n",
			wantKept:  1,
			wantWarns: []string{prefix + "jenkins.clouds[0].kubernetes.serverUrl, unclassified"},
		},
		{
			name:      "templates is not a list",
			doc:       "jenkins:\n  clouds:\n    - kubernetes:\n        templates: hello\n",
			wantWarns: []string{prefix + "jenkins.clouds[0].kubernetes.templates"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lift := liftTenantPodTemplates("payments", "payments-ns", tc.doc, nil)
			if len(lift.Templates) != tc.wantKept {
				t.Errorf("kept %d templates, want %d", len(lift.Templates), tc.wantKept)
			}
			if got := warningMessages(lift.Warnings, v1alpha1.TeamReasonBundleKeysIgnored); !reflect.DeepEqual(got, tc.wantWarns) {
				t.Errorf("warnings = %q, want %q", got, tc.wantWarns)
			}
		})
	}
}

func TestLiftTenantPodTemplates_Unparseable(t *testing.T) {
	for _, doc := range []string{"jenkins: [unclosed\n", "- a\n- b\n"} {
		lift := liftTenantPodTemplates("payments", "payments-ns", doc, nil)
		if len(lift.Templates) != 0 {
			t.Errorf("%q: expected no template, got %+v", doc, lift.Templates)
		}
		msgs := warningMessages(lift.Warnings, v1alpha1.TeamReasonBundleKeysIgnored)
		if len(msgs) != 1 || !strings.HasPrefix(msgs[0], `tenant bundle key "jenkins.yaml" does not parse: `) {
			t.Errorf("%q: unexpected warnings %q", doc, msgs)
		}
	}
}
