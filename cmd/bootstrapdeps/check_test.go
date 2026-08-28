package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLock(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lock.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	return path
}

const goodLock = `baseline: "2.555"
sets:
  2.555:
    core:
      - mailer
    plugins:
      - artifactId: mailer
        version: "534.v9"
      - artifactId: jakarta-mail-api
        version: "2.1.5-1"
    bootstrap:
      - artifactId: varroa-mite-auth
        version: "1.0-SNAPSHOT"
      - artifactId: mailer
        version: "534.v9"
        mins:
          - "534.v1"
      - artifactId: jakarta-mail-api
        version: "2.1.5-1"
        mins:
          - "2.1.3-2"
`

func TestRunCheck_Passes(t *testing.T) {
	var out bytes.Buffer
	if err := runCheck(writeLock(t, goodLock), &out); err != nil {
		t.Fatalf("runCheck: %v", err)
	}
	if !strings.Contains(out.String(), "2 member(s) verified") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

// TestRunCheck_MemberMissingFromPlugins is the failure D9 exists for: a lock
// refresh that drops a bootstrap member entirely.
func TestRunCheck_MemberMissingFromPlugins(t *testing.T) {
	lock := strings.Replace(goodLock,
		"      - artifactId: jakarta-mail-api\n        version: \"2.1.5-1\"\n    bootstrap:",
		"    bootstrap:", 1)
	var out bytes.Buffer
	err := runCheck(writeLock(t, lock), &out)
	if err == nil {
		t.Fatal("expected a failure for a bootstrap member absent from plugins")
	}
	if !strings.Contains(err.Error(), "1 bootstrap problem") {
		t.Errorf("error = %v", err)
	}
}

// TestRunCheck_PinMovedWithoutResolution catches a hand edit that changes a pin
// without re-running --resolve.
func TestRunCheck_PinMovedWithoutResolution(t *testing.T) {
	lock := strings.Replace(goodLock,
		"      - artifactId: mailer\n        version: \"534.v9\"\n      - artifactId: jakarta-mail-api",
		"      - artifactId: mailer\n        version: \"535.vNEW\"\n      - artifactId: jakarta-mail-api", 1)
	var out bytes.Buffer
	err := runCheck(writeLock(t, lock), &out)
	if err == nil {
		t.Fatal("expected a failure for a moved pin")
	}
}

// TestRunCheck_RootIsExempt proves the root is not subjected to either check:
// it is baked into the image and is never a lock member.
func TestRunCheck_RootIsExempt(t *testing.T) {
	var out bytes.Buffer
	if err := runCheck(writeLock(t, goodLock), &out); err != nil {
		t.Fatalf("the root must not be required in plugins: %v", err)
	}
}

func TestRunCheck_MissingBootstrapBlock(t *testing.T) {
	lock := `baseline: "2.555"
sets:
  2.555:
    plugins:
      - artifactId: mailer
        version: "534.v9"
`
	var out bytes.Buffer
	if err := runCheck(writeLock(t, lock), &out); err == nil {
		t.Fatal("expected a failure for a set with no bootstrap block")
	}
}

func TestRunCheck_UnreadableLock(t *testing.T) {
	var out bytes.Buffer
	if err := runCheck(filepath.Join(t.TempDir(), "nope.yaml"), &out); err == nil {
		t.Fatal("expected a failure for a missing lock file")
	}
}

// TestRunCheck_CommittedLock is the gate itself: the lock in this repository
// must satisfy the assertion.
func TestRunCheck_CommittedLock(t *testing.T) {
	var out bytes.Buffer
	if err := runCheck("../../internal/controller/pluginlock/lock.yaml", &out); err != nil {
		t.Fatalf("the committed lock fails its own bootstrap assertion: %v", err)
	}
}
