package runners

import (
	"reflect"
	"strings"
	"testing"
)

// TestRenderNode_NoDeclaredLabels matches design D3's scenario "A runner
// with no declared label still gets one".
func TestRenderNode_NoDeclaredLabels(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	node := renderNode(runner)
	permanent := node["permanent"].(map[string]any)
	if got, want := permanent["labelString"], "varroa-tenant-payments payments-runner"; got != want {
		t.Fatalf("labels = %q, want %q", got, want)
	}
}

// TestRenderNode_DeclaredLabelsPrefixed matches "Labels are prefixed by the
// operator".
func TestRenderNode_DeclaredLabelsPrefixed(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	runner.Spec.Labels = []string{"windows", "msbuild"}
	node := renderNode(runner)
	permanent := node["permanent"].(map[string]any)
	want := "varroa-tenant-payments payments-runner payments-windows payments-msbuild"
	if got := permanent["labelString"]; got != want {
		t.Fatalf("labels = %q, want %q", got, want)
	}
}

// TestRenderNode_CannotForgeReservedToken matches "A tenant cannot forge the
// reserved token": a declared label matching the reserved token's own shape
// is rendered as an ordinary <tenant>-prefixed label, never at the reserved
// position.
func TestRenderNode_CannotForgeReservedToken(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	runner.Spec.Labels = []string{"varroa-tenant-search"}
	node := renderNode(runner)
	permanent := node["permanent"].(map[string]any)
	want := "varroa-tenant-payments payments-runner payments-varroa-tenant-search"
	if got := permanent["labelString"]; got != want {
		t.Fatalf("labels = %q, want %q", got, want)
	}
}

func TestRenderNode_SharedFields(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	runner.Spec.NumExecutors = 2
	node := renderNode(runner)
	permanent := node["permanent"].(map[string]any)

	if permanent["name"] != "payments-win-1" {
		t.Fatalf("name = %v, want payments-win-1", permanent["name"])
	}
	if permanent["remoteFS"] != "/home/jenkins" {
		t.Fatalf("remoteFS = %v", permanent["remoteFS"])
	}
	if permanent["numExecutors"] != 2 {
		t.Fatalf("numExecutors = %v, want 2", permanent["numExecutors"])
	}
	if permanent["mode"] != "EXCLUSIVE" {
		t.Fatalf("mode = %v, want EXCLUSIVE", permanent["mode"])
	}
	if permanent["retentionStrategy"] != "always" {
		t.Fatalf("retentionStrategy = %v, want always", permanent["retentionStrategy"])
	}
}

func TestRenderNode_SSHLauncher_PortDefaultsTo22(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	node := renderNode(runner)
	launcher := node["permanent"].(map[string]any)["launcher"].(map[string]any)
	ssh := launcher["ssh"].(map[string]any)
	if ssh["port"] != 22 {
		t.Fatalf("port = %v, want 22 (nil spec.port fallback)", ssh["port"])
	}
	if ssh["credentialsId"] != "varroa-runner-win-1" {
		t.Fatalf("credentialsId = %v, want varroa-runner-win-1", ssh["credentialsId"])
	}
	if _, hasJavaPath := ssh["javaPath"]; hasJavaPath {
		t.Fatal("javaPath present when spec.javaPath is unset")
	}
}

func TestRenderNode_SSHLauncher_ExplicitPortAndJavaPath(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	port := 2222
	runner.Spec.Port = &port
	runner.Spec.JavaPath = `C:\jdk\bin\java.exe`
	node := renderNode(runner)
	ssh := node["permanent"].(map[string]any)["launcher"].(map[string]any)["ssh"].(map[string]any)
	if ssh["port"] != 2222 {
		t.Fatalf("port = %v, want 2222", ssh["port"])
	}
	if ssh["javaPath"] != `C:\jdk\bin\java.exe` {
		t.Fatalf("javaPath = %v", ssh["javaPath"])
	}
}

func TestRenderNode_InboundLauncher(t *testing.T) {
	runner := inboundRunner()
	node := renderNode(runner)
	launcher := node["permanent"].(map[string]any)["launcher"].(map[string]any)
	inbound, ok := launcher["inbound"].(map[string]any)
	if !ok {
		t.Fatalf("launcher = %+v, want an inbound block", launcher)
	}
	if inbound["webSocket"] != true {
		t.Fatal("webSocket not set true for an inbound launcher")
	}
	if _, hasSSH := launcher["ssh"]; hasSSH {
		t.Fatal("inbound launcher also carries an ssh block")
	}
}

// TestRenderNode_SSHLauncher_MatchesDesignYAML is task 4.1/4.2's
// golden-file acceptance: the whole rendered "permanent" entry, field for
// field, against design D3's own worked example (the payments/win-1
// runner).
func TestRenderNode_SSHLauncher_MatchesDesignYAML(t *testing.T) {
	runner := sshRunner("win-1", "payments")
	runner.Spec.NumExecutors = 2
	runner.Spec.RemoteFS = `C:\J`
	runner.Spec.Host = "10.20.0.5"
	runner.Spec.JavaPath = `C:\Program Files\Eclipse Adoptium\jdk-21\bin\java.exe`
	runner.Spec.Labels = []string{"windows", "msbuild"}

	node := renderNode(runner)
	permanent, ok := node["permanent"].(map[string]any)
	if !ok {
		t.Fatalf("node = %+v, want a \"permanent\" entry", node)
	}

	want := map[string]any{
		"name":              "payments-win-1",
		"labelString":       "varroa-tenant-payments payments-runner payments-windows payments-msbuild",
		"remoteFS":          `C:\J`,
		"numExecutors":      2,
		"mode":              "EXCLUSIVE",
		"retentionStrategy": "always",
		"launcher": map[string]any{
			"ssh": map[string]any{
				"host":          "10.20.0.5",
				"port":          22,
				"credentialsId": "varroa-runner-win-1",
				"javaPath":      `C:\Program Files\Eclipse Adoptium\jdk-21\bin\java.exe`,
				"sshHostKeyVerificationStrategy": map[string]any{
					"manuallyTrustedKeyVerificationStrategy": map[string]any{
						"requireInitialManualTrust": false,
					},
				},
			},
		},
	}

	if !reflect.DeepEqual(permanent, want) {
		t.Fatalf("permanent = %+v,\nwant %+v", permanent, want)
	}
}

// TestRenderNode_InboundLauncher_MatchesDesignYAML is task 4.2's
// golden-file acceptance for the inbound launcher block.
func TestRenderNode_InboundLauncher_MatchesDesignYAML(t *testing.T) {
	runner := inboundRunner()
	launcher := renderNode(runner)["permanent"].(map[string]any)["launcher"]
	want := map[string]any{"inbound": map[string]any{"webSocket": true}}
	if !reflect.DeepEqual(launcher, want) {
		t.Fatalf("launcher = %+v, want %+v", launcher, want)
	}
}

// TestRenderNode_OnlyOwningTenantsReservedTokenAppears is task 4.6's Go-side
// fence-interaction acceptance: a node rendered for tenant A carries exactly
// the reserved token varroa-tenant-A plus A's prefixed user labels, and no
// reserved token naming any other tenant. C5's dispatcher (plugin side, task
// 4.5) is what actually fences a build on this token; this proves the
// renderer never hands it a competing or forged one.
func TestRenderNode_OnlyOwningTenantsReservedTokenAppears(t *testing.T) {
	runner := sshRunner("agent-1", "alpha")
	runner.Spec.Labels = []string{"linux"}

	node := renderNode(runner)
	labelStr := node["permanent"].(map[string]any)["labelString"].(string)
	labels := strings.Fields(labelStr)

	want := []string{"varroa-tenant-alpha", "alpha-runner", "alpha-linux"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	for _, l := range labels {
		if strings.HasPrefix(l, "varroa-tenant-") && l != "varroa-tenant-alpha" {
			t.Fatalf("label %q names a tenant other than alpha", l)
		}
	}
	if strings.Contains(labelStr, "varroa-tenant-bravo") {
		t.Fatal("node carries a reserved token for tenant bravo")
	}
}

func TestReservedTenantLabel(t *testing.T) {
	if got, want := reservedTenantLabel("payments"), "varroa-tenant-payments"; got != want {
		t.Fatalf("reservedTenantLabel = %q, want %q", got, want)
	}
}
