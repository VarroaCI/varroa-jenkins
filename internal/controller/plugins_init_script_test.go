package controller

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestPluginsInitScriptExitsOnTERM runs the plugins-init script under sh with
// a jenkins-plugin-cli stub that blocks. A TERM to the shell must stop the
// stub and end the script with status 143. The shell runs in its own process
// group because the trap signals the whole group.
func TestPluginsInitScriptExitsOnTERM(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	stub := "#!/bin/sh\ntouch " + started + "\nsleep 60\n"
	if err := os.WriteFile(filepath.Join(dir, "jenkins-plugin-cli"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", pluginsInitScript)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the jenkins-plugin-cli stub did not start; output: %s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
			t.Fatalf("script exit = %v, want exit status 143; output: %s", err, out.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("script did not exit within 3s of TERM; output: %s", strings.TrimSpace(out.String()))
	}
}
