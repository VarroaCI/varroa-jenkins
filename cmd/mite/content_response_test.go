package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildContentResponse_RedactsCredentialValues(t *testing.T) {
	dir := t.TempDir()
	items := `items:
  - kind: folder
    name: payments
    properties:
      - folderCredentialsProperty:
          folderCredentials:
            - credentials:
                - secretText:
                    id: gh
                    scope: GLOBAL
                    secret: tenant-secret-value
`
	jcasc := `credentials:
  system:
    domainCredentials:
      - credentials:
          - basicSSHUserPrivateKey:
              id: varroa-runner-r1
              scope: SYSTEM
              username: builder
              privateKeySource:
                directEntry:
                  privateKey: runner-key-body
`
	plugins := "git:5.2.0\n"
	for name, body := range map[string]string{
		"last-items.yaml":  items,
		"last-jcasc.yaml":  jcasc,
		"last-plugins.txt": plugins,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	resp := buildContentResponse(dir, "req-1")

	if resp.RequestId != "req-1" {
		t.Errorf("request id = %q", resp.RequestId)
	}
	if strings.Contains(resp.ItemsYaml, "tenant-secret-value") || !strings.Contains(resp.ItemsYaml, "<redacted>") {
		t.Errorf("items answer is not redacted:\n%s", resp.ItemsYaml)
	}
	if strings.Contains(resp.JcascYaml, "runner-key-body") || !strings.Contains(resp.JcascYaml, "<redacted>") {
		t.Errorf("jcasc answer is not redacted:\n%s", resp.JcascYaml)
	}
	if resp.PluginsTxt != plugins {
		t.Errorf("plugins answer = %q, want %q", resp.PluginsTxt, plugins)
	}

	onDisk, err := os.ReadFile(filepath.Join(dir, "last-items.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != items {
		t.Errorf("the cached items file changed; the convergence hash reads it")
	}
}
