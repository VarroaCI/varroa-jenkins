package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestBFFDoesNotImportForgeClient checks that no BFF package imports the forge
// client. The BFF reaches the forge only through the operator relay, which
// uses the wire types of forgeapi.
func TestBFFDoesNotImportForgeClient(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/varroaci/varroa-jenkins/cmd/bff").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if dep == "github.com/varroaci/varroa-jenkins/internal/forge" {
			t.Fatal("the BFF depends on internal/forge, which only the operator may import")
		}
	}
}
