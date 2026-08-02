package main

import (
	"sort"
	"strings"
	"testing"
)

func TestManagedPaths_Empty(t *testing.T) {
	paths, err := managedPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Errorf("expected no paths for empty YAML, got %d", len(paths))
	}
}

func TestManagedPaths_ScalarLeaves(t *testing.T) {
	y := `
jenkins:
  systemMessage: "hello"
  numExecutors: 2
`
	paths, err := managedPaths(y)
	if err != nil {
		t.Fatal(err)
	}
	want := sortedPaths("jenkins.numExecutors", "jenkins.systemMessage")
	if !pathsEqual(paths, want) {
		t.Errorf("got %v, want %v", pathStrings(paths), pathStrings(want))
	}
}

func TestManagedPaths_NestedMaps(t *testing.T) {
	y := `
jenkins:
  securityRealm:
    local:
      allowsSignup: false
`
	paths, err := managedPaths(y)
	if err != nil {
		t.Fatal(err)
	}
	want := sortedPaths("jenkins.securityRealm.local.allowsSignup")
	if !pathsEqual(paths, want) {
		t.Errorf("got %v, want %v", pathStrings(paths), pathStrings(want))
	}
}

func TestManagedPaths_ListCapturedWhole(t *testing.T) {
	y := `
tool:
  git:
    installations:
    - name: "Default"
      home: "/usr/bin/git"
`
	paths, err := managedPaths(y)
	if err != nil {
		t.Fatal(err)
	}
	want := sortedPaths("tool.git.installations")
	if !pathsEqual(paths, want) {
		t.Errorf("got %v, want %v", pathStrings(paths), pathStrings(want))
	}
}

func TestProjectAndHash_Stable(t *testing.T) {
	y := `
jenkins:
  systemMessage: "hello"
  numExecutors: 2
`
	h1, err := projectAndHash(y, y)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := projectAndHash(y, y)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("identical config produced different hashes: %s vs %s", h1, h2)
	}
}

func TestProjectAndHash_ValueChange(t *testing.T) {
	applied := `
jenkins:
  systemMessage: "hello"
`
	export1 := applied
	export2 := `
jenkins:
  systemMessage: "world"
`
	h1, err := projectAndHash(export1, applied)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := projectAndHash(export2, applied)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("value change should produce different hash")
	}
}

func TestProjectAndHash_NonManagedKeyUnchanged(t *testing.T) {
	applied := `
jenkins:
  systemMessage: "hello"
`
	export := `
jenkins:
  systemMessage: "hello"
  unclassified:
    somePlugin:
      setting: true
`
	h1, err := projectAndHash(export, applied)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := projectAndHash(export, applied)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Error("non-managed key should not affect hash")
	}
}

func TestProjectAndHash_RemovedManagedKeyChangesHash(t *testing.T) {
	applied := `
jenkins:
  systemMessage: "hello"
  numExecutors: 2
`
	export1 := `
jenkins:
  systemMessage: "hello"
  numExecutors: 2
`
	export2 := `
jenkins:
  systemMessage: "hello"
`
	h1, err := projectAndHash(export1, applied)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := projectAndHash(export2, applied)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("removed managed key should change hash")
	}
}

func TestProjectAndHash_ReorderedListChangesHash(t *testing.T) {
	applied := `
tool:
  git:
    installations:
    - name: "A"
    - name: "B"
`
	export1 := applied
	export2 := `
tool:
  git:
    installations:
    - name: "B"
    - name: "A"
`
	h1, err := projectAndHash(export1, applied)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := projectAndHash(export2, applied)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("reordered list should change hash")
	}
}

func sortedPaths(ps ...string) []nodePath {
	out := make([]nodePath, 0, len(ps))
	for _, p := range ps {
		out = append(out, strings.Split(p, "."))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].String() < out[j].String()
	})
	return out
}

func pathsEqual(a, b []nodePath) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].String() != b[i].String() {
			return false
		}
	}
	return true
}

func TestFingerprintAuthzSetOrderInsensitive(t *testing.T) {
	// YAML that declares the authz role buckets with multiple roles, permissions, and
	// entries to exercise real list reordering (not just map-key ordering).
	appliedYAML := `
jenkins:
  authorizationStrategy:
    roleBased:
      roles:
        global:
          - name: "admin"
            permissions:
              - "Overall/Administer"
              - "Overall/Read"
              - "Overall/SystemRead"
            entries:
              - type: "user"
                name: "alice"
              - type: "group"
                name: "varroa-admins"
        item:
          - name: "developer"
            permissions:
              - "Job/Build"
              - "Job/Read"
            entries:
              - type: "user"
                name: "bob"
          - name: "viewer"
            permissions:
              - "Job/Read"
            entries:
              - type: "user"
                name: "eve"
        agent:
          - name: "agent-reader"
            permissions:
              - "Agent/Build"
              - "Agent/Connect"
            entries:
              - type: "user"
                name: "charlie"
`

	// Same document — roles, permissions, entries in identical order.
	hash1, err := projectAndHash(appliedYAML, appliedYAML)
	if err != nil {
		t.Fatalf("hash1: %v", err)
	}

	// Reorder: reverse role order, reverse permissions within each role, reverse entries.
	exportReordered := `
jenkins:
  authorizationStrategy:
    roleBased:
      roles:
        agent:
          - name: "agent-reader"
            entries:
              - type: "user"
                name: "charlie"
            permissions:
              - "Agent/Connect"
              - "Agent/Build"
        item:
          - name: "viewer"
            entries:
              - type: "user"
                name: "eve"
            permissions:
              - "Job/Read"
          - name: "developer"
            entries:
              - type: "user"
                name: "bob"
            permissions:
              - "Job/Read"
              - "Job/Build"
        global:
          - name: "admin"
            entries:
              - type: "group"
                name: "varroa-admins"
              - type: "user"
                name: "alice"
            permissions:
              - "Overall/SystemRead"
              - "Overall/Read"
              - "Overall/Administer"
`

	hash2, err := projectAndHash(exportReordered, appliedYAML)
	if err != nil {
		t.Fatalf("hash2: %v", err)
	}

	if hash1 != hash2 {
		t.Error("reordering authz roles/permissions/entries should NOT change fingerprint")
	}

	// Remove a permission → hash should differ.
	exportRemovedPerm := `
jenkins:
  authorizationStrategy:
    roleBased:
      roles:
        global:
          - name: "admin"
            permissions:
              - "Overall/Administer"
              - "Overall/Read"
            entries:
              - type: "user"
                name: "alice"
              - type: "group"
                name: "varroa-admins"
        item:
          - name: "developer"
            permissions:
              - "Job/Build"
              - "Job/Read"
            entries:
              - type: "user"
                name: "bob"
          - name: "viewer"
            permissions:
              - "Job/Read"
            entries:
              - type: "user"
                name: "eve"
        agent:
          - name: "agent-reader"
            permissions:
              - "Agent/Build"
              - "Agent/Connect"
            entries:
              - type: "user"
                name: "charlie"
`
	hash3, err := projectAndHash(exportRemovedPerm, appliedYAML)
	if err != nil {
		t.Fatalf("hash3: %v", err)
	}
	if hash1 == hash3 {
		t.Error("removing Overall/SystemRead permission should change fingerprint")
	}

	// Add a role → hash should differ.
	exportAddedRole := `
jenkins:
  authorizationStrategy:
    roleBased:
      roles:
        global:
          - name: "admin"
            permissions:
              - "Overall/Administer"
              - "Overall/Read"
              - "Overall/SystemRead"
            entries:
              - type: "user"
                name: "alice"
              - type: "group"
                name: "varroa-admins"
          - name: "limited-admin"
            permissions:
              - "Overall/Read"
            entries:
              - type: "user"
                name: "dave"
        item:
          - name: "developer"
            permissions:
              - "Job/Build"
              - "Job/Read"
            entries:
              - type: "user"
                name: "bob"
          - name: "viewer"
            permissions:
              - "Job/Read"
            entries:
              - type: "user"
                name: "eve"
        agent:
          - name: "agent-reader"
            permissions:
              - "Agent/Build"
              - "Agent/Connect"
            entries:
              - type: "user"
                name: "charlie"
`
	hash4, err := projectAndHash(exportAddedRole, appliedYAML)
	if err != nil {
		t.Fatalf("hash4: %v", err)
	}
	if hash1 == hash4 {
		t.Error("adding a role should change fingerprint")
	}

	// Reorder a non-set list (e.g. tool.git.installations) should still change hash.
	appliedTool := `
tool:
  git:
    installations:
      - name: "A"
        home: "/usr/bin/git"
      - name: "B"
        home: "/usr/local/bin/git"
`
	exportToolReordered := `
tool:
  git:
    installations:
      - name: "B"
        home: "/usr/local/bin/git"
      - name: "A"
        home: "/usr/bin/git"
`
	hTool1, err := projectAndHash(appliedTool, appliedTool)
	if err != nil {
		t.Fatalf("tool h1: %v", err)
	}
	hTool2, err := projectAndHash(exportToolReordered, appliedTool)
	if err != nil {
		t.Fatalf("tool h2: %v", err)
	}
	if hTool1 == hTool2 {
		t.Error("reordering a non-set managed list should change fingerprint")
	}
}

func TestMatchSetPath(t *testing.T) {
	p := nodePath{"jenkins", "authorizationStrategy", "roleBased", "roles", "global"}
	sp, ok := matchSetPath(p)
	if !ok {
		t.Fatal("expected match for global role path")
	}
	if len(sp.innerSetKeys) != 2 {
		t.Fatalf("expected 2 inner set keys, got %d", len(sp.innerSetKeys))
	}

	// Too few segments.
	sp2, ok2 := matchSetPath(nodePath{"jenkins", "authorizationStrategy", "roleBased", "roles"})
	if ok2 {
		t.Fatal("expected no match for 4-segment path")
	}
	_ = sp2

	// Unrelated path.
	sp3, ok3 := matchSetPath(nodePath{"tool", "git", "installations"})
	if ok3 {
		t.Fatal("expected no match for unrelated path")
	}
	_ = sp3
}

func pathStrings(ps []nodePath) []string {
	s := make([]string, 0, len(ps))
	for _, p := range ps {
		s = append(s, p.String())
	}
	return s
}
