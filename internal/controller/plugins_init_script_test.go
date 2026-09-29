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

// pluginsInitHarness runs pluginsInitScript under sh with stub
// jenkins-plugin-cli, curl, and sleep binaries first on PATH.
type pluginsInitHarness struct {
	dir     string
	termLog string
	calls   string
	curlLog string
}

const stubCLI = `#!/bin/sh
echo run >> "$STUB_CALLS"
case "$STUB_CLI_MODE" in
fallback|fallback-fail)
  echo "Unable to resolve plugin URL https://uc.example.com/download/git/5.2.0/git.hpi, or download plugin git to file: status code: 404, reason phrase: Not Found"
  echo "Downloading from mirrors failed, falling back to https://archives.jenkins.io/"
  echo "Unable to resolve plugin URL https://uc.example.com/download/credentials/1.0/credentials.hpi, or download plugin credentials to file: status code: 404, reason phrase: Not Found"
  echo "Downloading from mirrors failed, falling back to https://archives.jenkins.io/"
  echo "Unable to resolve plugin URL https://archives.jenkins.io/plugins/credentials/1.0/credentials.hpi, or download plugin credentials to file: status code: 404"
  [ "$STUB_CLI_MODE" = fallback ] && exit 0
  exit 1
  ;;
*)
  echo "git downloaded successfully"
  exit 0
  ;;
esac
`

const stubCurl = `#!/bin/sh
echo "$@" >> "$STUB_CURL_LOG"
printf '%s' "$STUB_CURL_CODE"
[ "$STUB_CURL_CODE" = "000" ] && exit 28
exit 0
`

func newPluginsInitHarness(t *testing.T) *pluginsInitHarness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"jenkins-plugin-cli": stubCLI,
		"curl":               stubCurl,
		"sleep":              "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plugins := "# managed\ngit:5.2.0\ncredentials:1.0\n"
	if err := os.WriteFile(filepath.Join(dir, "plugins.txt"), []byte(plugins), 0o644); err != nil {
		t.Fatal(err)
	}
	return &pluginsInitHarness{
		dir:     dir,
		termLog: filepath.Join(dir, "termination-log"),
		calls:   filepath.Join(dir, "calls"),
		curlLog: filepath.Join(dir, "curl-log"),
	}
}

func (h *pluginsInitHarness) run(t *testing.T, env ...string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", pluginsInitScript)
	cmd.Env = append([]string{
		"PATH=" + filepath.Join(h.dir, "bin") + ":" + os.Getenv("PATH"),
		"TMPDIR=" + h.dir,
		"VARROA_PLUGINS_FILE=" + filepath.Join(h.dir, "plugins.txt"),
		"VARROA_PLUGIN_DIR=" + filepath.Join(h.dir, "plugins"),
		"VARROA_TERMINATION_LOG=" + h.termLog,
		"STUB_CALLS=" + h.calls,
		"STUB_CURL_LOG=" + h.curlLog,
	}, env...)
	out, err := cmd.CombinedOutput()
	t.Logf("script output:\n%s", out)
	if err == nil {
		return 0
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run script: %v", err)
	}
	return exitErr.ExitCode()
}

func (h *pluginsInitHarness) read(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func (h *pluginsInitHarness) cliRuns() int {
	return strings.Count(h.read(h.calls), "run")
}

func TestPluginsInitScript_NotFoundDownloadURLExits42WithoutRetry(t *testing.T) {
	h := newPluginsInitHarness(t)
	code := h.run(t, "JENKINS_UC_DOWNLOAD_URL=https://uc.example.com/download/", "STUB_CURL_CODE=404", "STUB_CLI_MODE=clean")
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
	if n := h.cliRuns(); n != 0 {
		t.Fatalf("jenkins-plugin-cli ran %d times, want 0", n)
	}
	want := "varroa:download-url-not-found url=https://uc.example.com/download/git/5.2.0/git.hpi plugin=git:5.2.0 status=404"
	if got := strings.TrimSpace(h.read(h.termLog)); got != want {
		t.Fatalf("termination log = %q, want %q", got, want)
	}
	if curl := h.read(h.curlLog); !strings.Contains(curl, "0-0") {
		t.Fatalf("curl args %q do not request a one-byte range", curl)
	}
}

func TestPluginsInitScript_GoneDownloadURLExits42(t *testing.T) {
	h := newPluginsInitHarness(t)
	code := h.run(t, "JENKINS_UC_DOWNLOAD_URL=https://uc.example.com/download", "STUB_CURL_CODE=410")
	if code != 42 {
		t.Fatalf("exit code = %d, want 42", code)
	}
}

func TestPluginsInitScript_PreflightNetworkErrorContinues(t *testing.T) {
	h := newPluginsInitHarness(t)
	code := h.run(t, "JENKINS_UC_DOWNLOAD_URL=https://uc.example.com/download/plugins", "STUB_CURL_CODE=000", "STUB_CLI_MODE=clean")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if n := h.cliRuns(); n != 1 {
		t.Fatalf("jenkins-plugin-cli ran %d times, want 1", n)
	}
	if got := h.read(h.termLog); got != "" {
		t.Fatalf("termination log = %q, want empty", got)
	}
}

func TestPluginsInitScript_NoPreflightWithoutDownloadURL(t *testing.T) {
	h := newPluginsInitHarness(t)
	code := h.run(t, "STUB_CURL_CODE=404", "STUB_CLI_MODE=clean")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if curl := h.read(h.curlLog); curl != "" {
		t.Fatalf("curl ran with %q, want no pre-flight", curl)
	}
}

func TestPluginsInitScript_ArchiveFallbackWritesMarkerOnSuccess(t *testing.T) {
	h := newPluginsInitHarness(t)
	code := h.run(t, "JENKINS_UC_DOWNLOAD_URL=https://uc.example.com/download", "STUB_CURL_CODE=206", "STUB_CLI_MODE=fallback")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	want := "varroa:archive-fallback count=2 plugin=git url=https://uc.example.com/download/git/5.2.0/git.hpi"
	if got := strings.TrimSpace(h.read(h.termLog)); got != want {
		t.Fatalf("termination log = %q, want %q", got, want)
	}
}

func TestPluginsInitScript_ArchiveFallbackMarkerOnFinalFailure(t *testing.T) {
	h := newPluginsInitHarness(t)
	code := h.run(t, "STUB_CLI_MODE=fallback-fail")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if n := h.cliRuns(); n != 5 {
		t.Fatalf("jenkins-plugin-cli ran %d times, want 5", n)
	}
	if got := h.read(h.termLog); !strings.HasPrefix(got, "varroa:archive-fallback count=2 plugin=git ") {
		t.Fatalf("termination log = %q, want an archive-fallback marker", got)
	}
}

func TestPluginsInitScript_CleanRunLeavesEmptyMarker(t *testing.T) {
	h := newPluginsInitHarness(t)
	if err := os.WriteFile(h.termLog, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := h.run(t, "JENKINS_UC_DOWNLOAD_URL=https://uc.example.com/download/plugins", "STUB_CURL_CODE=200", "STUB_CLI_MODE=clean")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := h.read(h.termLog); got != "" {
		t.Fatalf("termination log = %q, want empty", got)
	}
}

func TestPluginsInitScript_UnwritableTerminationLogFallsBackToStderr(t *testing.T) {
	h := newPluginsInitHarness(t)
	cmd := exec.Command("sh", "-c", pluginsInitScript)
	cmd.Env = []string{
		"PATH=" + filepath.Join(h.dir, "bin") + ":" + os.Getenv("PATH"),
		"TMPDIR=" + h.dir,
		"VARROA_PLUGINS_FILE=" + filepath.Join(h.dir, "plugins.txt"),
		"VARROA_TERMINATION_LOG=" + filepath.Join(h.dir, "missing", "termination-log"),
		"STUB_CALLS=" + h.calls,
		"STUB_CURL_LOG=" + h.curlLog,
		"JENKINS_UC_DOWNLOAD_URL=https://uc.example.com/download",
		"STUB_CURL_CODE=404",
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_ = cmd.Run()
	if !strings.Contains(stderr.String(), "varroa:download-url-not-found ") {
		t.Fatalf("stderr %q does not carry the marker", stderr.String())
	}
}
