package bundle

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInjectTenantClouds_RawTemplate(t *testing.T) {
	defaultTemplate := TenantCloudTemplate{
		Name:           "team-a-default",
		Namespace:      "team-a-ns",
		ServiceAccount: "varroa-agent-team-a",
		Labels:         "varroa-tenant-team-a team-a-default",
		Containers: []TenantCloudContainer{
			{Name: "jnlp", Image: "jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21", WorkingDir: "/home/jenkins/agent"},
		},
	}
	rawContainers := []any{map[string]any{"name": "golang", "image": "golang:1.25"}}
	rawVolumes := []any{map[string]any{"emptyDirVolume": map[string]any{"mountPath": "/go"}}}
	liftedTemplate := TenantCloudTemplate{
		Name:           "team-a-golang",
		Namespace:      "team-a-ns",
		ServiceAccount: "varroa-agent-team-a",
		Labels:         "varroa-tenant-team-a team-a-golang",
		Raw: map[string]any{
			"containers":     rawContainers,
			"volumes":        rawVolumes,
			"idleMinutes":    5,
			"yaml":           "spec:\n  hostNetwork: false\n",
			"name":           "golang",
			"namespace":      "kube-system",
			"serviceAccount": "admin",
			"label":          "golang",
		},
	}

	out, err := InjectTenantClouds("", []TenantCloud{{
		Name:      "team-a",
		Namespace: "team-a-ns",
		Templates: []TenantCloudTemplate{defaultTemplate, liftedTemplate},
	}})
	if err != nil {
		t.Fatalf("InjectTenantClouds: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	k8s := doc["jenkins"].(map[string]any)["clouds"].([]any)[0].(map[string]any)["kubernetes"].(map[string]any)
	templates := k8s["templates"].([]any)
	if len(templates) != 2 {
		t.Fatalf("expected two templates, got %d: %v", len(templates), templates)
	}

	wantDefault := map[string]any{
		"name":           "team-a-default",
		"namespace":      "team-a-ns",
		"serviceAccount": "varroa-agent-team-a",
		"label":          "varroa-tenant-team-a team-a-default",
		"containers": []any{map[string]any{
			"name":                  "jnlp",
			"image":                 "jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21",
			"resourceRequestCpu":    "",
			"resourceRequestMemory": "",
			"resourceLimitCpu":      "",
			"resourceLimitMemory":   "",
			"workingDir":            "/home/jenkins/agent",
		}},
	}
	if !reflect.DeepEqual(templates[0], wantDefault) {
		t.Errorf("default template changed:\n got %#v\nwant %#v", templates[0], wantDefault)
	}

	wantLifted := map[string]any{
		"name":           "team-a-golang",
		"namespace":      "team-a-ns",
		"serviceAccount": "varroa-agent-team-a",
		"label":          "varroa-tenant-team-a team-a-golang",
		"containers":     rawContainers,
		"volumes":        rawVolumes,
		"idleMinutes":    5,
		"yaml":           "spec:\n  hostNetwork: false\n",
	}
	if !reflect.DeepEqual(templates[1], wantLifted) {
		t.Errorf("lifted template:\n got %#v\nwant %#v", templates[1], wantLifted)
	}
	if _, ok := templates[1].(map[string]any)["labels"]; ok {
		t.Error("expected no labels key on the lifted template")
	}
	if liftedTemplate.Raw["name"] != "golang" {
		t.Error("expected the renderer to leave Raw unchanged")
	}
}

func TestInjectTenantClouds_Appends(t *testing.T) {
	out, err := InjectTenantClouds("", []TenantCloud{
		{
			Name:            "team-a",
			Namespace:       "team-a-ns",
			JenkinsURL:      "${varroa_controller_endpoint}",
			CredentialsID:   "varroa-cloud-team-a",
			UsageRestricted: true,
			WebSocket:       true,
			Templates: []TenantCloudTemplate{
				{
					Name:           "team-a-default",
					Namespace:      "team-a-ns",
					ServiceAccount: "varroa-agent-team-a",
					Labels:         "varroa-tenant-team-a team-a-default",
					Containers: []TenantCloudContainer{
						{Name: "jnlp", Image: "jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21", WorkingDir: "/home/jenkins/agent"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("InjectTenantClouds: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	jenkinsSection, _ := doc["jenkins"].(map[string]any)
	clouds, _ := jenkinsSection["clouds"].([]any)
	if len(clouds) != 1 {
		t.Fatalf("expected exactly one cloud, got %d: %v", len(clouds), clouds)
	}
	cloud := clouds[0].(map[string]any)
	k8s := cloud["kubernetes"].(map[string]any)
	if k8s["name"] != "team-a" {
		t.Errorf("expected cloud name team-a, got %v", k8s["name"])
	}
	if k8s["credentialsId"] != "varroa-cloud-team-a" {
		t.Errorf("expected credentialsId varroa-cloud-team-a, got %v", k8s["credentialsId"])
	}
	if k8s["usageRestricted"] != true {
		t.Errorf("expected usageRestricted true, got %v", k8s["usageRestricted"])
	}
	templates, _ := k8s["templates"].([]any)
	if len(templates) != 1 {
		t.Fatalf("expected one template, got %d", len(templates))
	}
}

func TestInjectTenantClouds_KeepsExistingCloud(t *testing.T) {
	base := "jenkins:\n  clouds:\n    - kubernetes:\n        name: shared\n        namespace: shared-ns\n"

	out, err := InjectTenantClouds(base, []TenantCloud{{Name: "team-a", Namespace: "team-a-ns"}})
	if err != nil {
		t.Fatalf("InjectTenantClouds: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	jenkinsSection, _ := doc["jenkins"].(map[string]any)
	clouds, _ := jenkinsSection["clouds"].([]any)
	if len(clouds) != 2 {
		t.Fatalf("expected the existing cloud plus the new one, got %d: %v", len(clouds), clouds)
	}
	names := map[string]bool{}
	for _, c := range clouds {
		cm := c.(map[string]any)
		k8s := cm["kubernetes"].(map[string]any)
		names[k8s["name"].(string)] = true
	}
	if !names["shared"] || !names["team-a"] {
		t.Errorf("expected both shared and team-a clouds, got %v", names)
	}
}

func TestInjectTenantClouds_RejectsNameCollisionWithExistingCloud(t *testing.T) {
	base := "jenkins:\n  clouds:\n    - kubernetes:\n        name: team-a\n        namespace: shared-ns\n"

	_, err := InjectTenantClouds(base, []TenantCloud{{Name: "team-a", Namespace: "team-a-ns"}})
	if err == nil {
		t.Fatal("expected an error for a tenant cloud name colliding with an existing cloud")
	}
	if !strings.Contains(err.Error(), "team-a") {
		t.Errorf("expected the error to name the colliding cloud, got: %v", err)
	}
}

func TestInjectTenantClouds_RejectsNameCollisionBetweenTenants(t *testing.T) {
	_, err := InjectTenantClouds("", []TenantCloud{
		{Name: "team-a", Namespace: "team-a-ns"},
		{Name: "team-a", Namespace: "team-a-ns-2"},
	})
	if err == nil {
		t.Fatal("expected an error for two tenant clouds sharing a name")
	}
}

func TestInjectTenantClouds_NoTenantsReturnsInputUnchanged(t *testing.T) {
	base := "jenkins:\n  numExecutors: 0\n"
	out, err := InjectTenantClouds(base, nil)
	if err != nil {
		t.Fatalf("InjectTenantClouds: %v", err)
	}
	if out != base {
		t.Errorf("expected the input returned unchanged, got %q", out)
	}
}

// TestInjectTenantClouds_AgentServiceURL asserts that the injector renders
// each tenant's own agent Service URL and keeps webSocket true. Two tenants
// on one cell therefore get two different jenkinsUrl values.
func TestInjectTenantClouds_AgentServiceURL(t *testing.T) {
	out, err := InjectTenantClouds("", []TenantCloud{
		{
			Name:       "payments",
			Namespace:  "payments-ns",
			JenkinsURL: "http://varroa-agents-payments-0.varroa-system.svc.cluster.local:8080",
			WebSocket:  true,
		},
		{
			Name:       "search",
			Namespace:  "search-ns",
			JenkinsURL: "http://varroa-agents-search-0.varroa-system.svc.cluster.local:8080",
			WebSocket:  true,
		},
	})
	if err != nil {
		t.Fatalf("InjectTenantClouds: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	jenkinsSection, _ := doc["jenkins"].(map[string]any)
	clouds, _ := jenkinsSection["clouds"].([]any)
	if len(clouds) != 2 {
		t.Fatalf("expected two clouds, got %d", len(clouds))
	}
	want := map[string]string{
		"payments": "http://varroa-agents-payments-0.varroa-system.svc.cluster.local:8080",
		"search":   "http://varroa-agents-search-0.varroa-system.svc.cluster.local:8080",
	}
	for _, entry := range clouds {
		k8s := entry.(map[string]any)["kubernetes"].(map[string]any)
		name, _ := k8s["name"].(string)
		if got := k8s["jenkinsUrl"]; got != want[name] {
			t.Errorf("%s jenkinsUrl = %v, want %v", name, got, want[name])
		}
		if k8s["webSocket"] != true {
			t.Errorf("%s webSocket = %v, want true", name, k8s["webSocket"])
		}
	}
}
