//go:build live

// Live probe for the challenge bundle.
//
// Reads the generated bundle-challenge/items.yaml, Flatten()s it, then for
// every expected path calls client.GetItemConfig(ctx, path) against a real
// Jenkins controller and asserts existence + correct root element.
//
// Authentication: Jenkins runs the VarroaSecurityRealm, which only accepts an
// operator-signed JWT (the same token the mite uses). The probe mints that
// token itself from the operator's signing-key Secret, so it needs nothing
// beyond a kubeconfig with read access to that Secret. Supply
// VARROA_JENKINS_TOKEN to skip minting and use a token you provide.
//
// Environment variables:
//
//	VARROA_CONTROLLER_NAME      — controller name (e.g. "my-controller")
//	VARROA_CONTROLLER_NAMESPACE — controller namespace (e.g. "varroa")
//	VARROA_JENKINS_URL          — Jenkins base URL (e.g. "https://jenkins.example.com")
//	VARROA_OPERATOR_NAMESPACE   — namespace holding the varroa-mite-signing-key
//	                              Secret (defaults to VARROA_CONTROLLER_NAMESPACE)
//	VARROA_JENKINS_TOKEN        — optional; use this Bearer token verbatim and
//	                              skip minting
//	KUBECONFIG                  — path to kubeconfig (defaults to ~/.kube/config)
//
// Usage:
//
//	go test -tags live ./internal/jenkins/items/... -run TestChallengeLive
package items

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/jenkins"
	"github.com/varroaci/varroa-jenkins/internal/mite"
)

func TestChallengeLive(t *testing.T) {
	ctx := context.Background()

	// Read env vars.
	controllerName := os.Getenv("VARROA_CONTROLLER_NAME")
	controllerNamespace := os.Getenv("VARROA_CONTROLLER_NAMESPACE")
	jenkinsURL := os.Getenv("VARROA_JENKINS_URL")

	if controllerName == "" || controllerNamespace == "" || jenkinsURL == "" {
		t.Fatal("VARROA_CONTROLLER_NAME, VARROA_CONTROLLER_NAMESPACE, and VARROA_JENKINS_URL must be set")
	}

	// Obtain a Bearer token the VarroaSecurityRealm will accept. Prefer an
	// explicit override; otherwise mint an operator-signed JWT from the
	// signing-key Secret, exactly as the operator does for each mite.
	token := os.Getenv("VARROA_JENKINS_TOKEN")
	if token == "" {
		operatorNamespace := os.Getenv("VARROA_OPERATOR_NAMESPACE")
		if operatorNamespace == "" {
			operatorNamespace = controllerNamespace
		}

		// NewClientsetClientWithKubeconfig needs an explicit path; it does not
		// apply the default loading rules. Fall back to ~/.kube/config when
		// KUBECONFIG is unset, mirroring kubectl's default.
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = filepath.Join(os.Getenv("HOME"), ".kube", "config")
		}
		cc, err := controller.NewClientsetClientWithKubeconfig(kubeconfig)
		if err != nil {
			t.Fatalf("build clientset from KUBECONFIG: %v", err)
		}
		sec, err := cc.GetSecret(ctx, "varroa-mite-signing-key", operatorNamespace)
		if err != nil {
			t.Fatalf("get varroa-mite-signing-key Secret in namespace %q (set VARROA_OPERATOR_NAMESPACE if the operator runs elsewhere): %v", operatorNamespace, err)
		}
		privPEM := sec["private-key"]
		if len(privPEM) == 0 {
			t.Fatalf("varroa-mite-signing-key Secret in namespace %q has no \"private-key\" entry", operatorNamespace)
		}
		signer, err := mite.NewMiteTokenSignerFromPEM(privPEM)
		if err != nil {
			t.Fatalf("load mite signing key: %v", err)
		}
		token, err = signer.GenerateMiteJenkinsToken(controllerName, controllerNamespace, 30*time.Minute)
		if err != nil {
			t.Fatalf("mint operator JWT: %v", err)
		}
	}

	client := jenkins.NewClient(jenkinsURL, "live-probe", token)

	// Load bundle-challenge/items.yaml.
	bundleData, err := os.ReadFile(filepath.Join("testdata", "challenge", "bundle-challenge", "items.yaml"))
	if err != nil {
		t.Fatalf("read bundle items.yaml: %v", err)
	}
	m, err := Parse(string(bundleData))
	if err != nil {
		t.Fatalf("Parse bundle: %v", err)
	}

	flat := m.Flatten()
	t.Logf("Expecting %d items from bundle-challenge", len(flat))

	// rootElement returns the config.xml opening root tag string for a given kind.
	rootElement := func(kind string) string {
		switch kind {
		case "folder":
			return "<com.cloudbees.hudson.plugins.folder.Folder"
		case "freeStyle":
			return "<project"
		case "pipeline":
			return "<flow-definition"
		case "multibranch":
			return "<org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject"
		case "organizationFolder":
			return "<jenkins.branch.OrganizationFolder"
		default:
			return "<" + kind
		}
	}

	var failures []string
	for _, ip := range flat {
		path := ip.Path
		kind := ip.Item.Kind
		rootTag := rootElement(kind)

		// Bounded retry for 503s.
		var xml string
		var exists bool
		var lastErr error
		for attempt := 0; attempt < 5; attempt++ {
			xml, exists, lastErr = client.GetItemConfig(ctx, path)
			if lastErr == nil {
				break
			}
			if strings.Contains(lastErr.Error(), "503") {
				time.Sleep(6 * time.Second)
				continue
			}
			break
		}

		if lastErr != nil {
			failures = append(failures, fmt.Sprintf("%s: GetItemConfig error: %v", path, lastErr))
			continue
		}
		if !exists {
			failures = append(failures, fmt.Sprintf("%s: does not exist", path))
			continue
		}
		if !strings.Contains(xml, rootTag) {
			failures = append(failures, fmt.Sprintf("%s: XML does not contain root tag %q", path, rootTag))
			continue
		}
		t.Logf("OK %s (%s)", path, kind)
	}

	if len(failures) > 0 {
		t.Errorf("%d item(s) failed probe:\n%s", len(failures), strings.Join(failures, "\n"))
	}
}
