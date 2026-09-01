package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/pluginresolve"
)

// fixtureHPI builds a genuine .hpi holding just a manifest.
func fixtureHPI(t *testing.T, shortName, version, deps string) []byte {
	t.Helper()
	mf := "Manifest-Version: 1.0\r\nShort-Name: " + shortName + "\r\nPlugin-Version: " + version + "\r\n"
	if deps != "" {
		mf += "Plugin-Dependencies: " + deps + "\r\n"
	}
	mf += "\r\n"

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("META-INF/MANIFEST.MF")
	if err != nil {
		t.Fatalf("create manifest entry: %v", err)
	}
	if _, err := w.Write([]byte(mf)); err != nil {
		t.Fatalf("write manifest entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// fakeFetcher serves fixture HPIs keyed by "name@version" and records what it
// was asked for, so a test can assert each HPI is fetched at most once.
type fakeFetcher struct {
	bodies map[string][]byte
	calls  []string
}

func (f *fakeFetcher) fetch(_ context.Context, name, version string) ([]byte, error) {
	key := name + "@" + version
	f.calls = append(f.calls, key)
	b, ok := f.bodies[key]
	if !ok {
		return nil, fmt.Errorf("no fixture for %s", key)
	}
	return b, nil
}

func TestWriteBootstrapYAML(t *testing.T) {
	var buf bytes.Buffer
	err := writeBootstrapYAML(&buf, []pluginresolve.BootstrapEntry{
		{ArtifactID: "varroa-mite-auth", Version: "1.0-SNAPSHOT"},
		{ArtifactID: "mailer", Version: "534.v9", Mins: []string{"534.v1"}},
	}, 4)
	if err != nil {
		t.Fatalf("writeBootstrapYAML: %v", err)
	}
	want := "    bootstrap:\n" +
		"      - artifactId: varroa-mite-auth\n" +
		"        version: \"1.0-SNAPSHOT\"\n" +
		"      - artifactId: mailer\n" +
		"        version: \"534.v9\"\n" +
		"        mins:\n" +
		"          - \"534.v1\"\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestRunResolve_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	hpiPath := filepath.Join(dir, "root.hpi")
	if err := os.WriteFile(hpiPath, fixtureHPI(t, "varroa-mite-auth", "1.0-SNAPSHOT (private-x)", "mailer:534.v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	pluginsPath := filepath.Join(dir, "plugins.txt")
	if err := os.WriteFile(pluginsPath, []byte("# comment\nmailer:534.v9\nother:1.0\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ff := &fakeFetcher{bodies: map[string][]byte{
		"mailer@534.v9": fixtureHPI(t, "mailer", "534.v9", ""),
	}}

	var buf bytes.Buffer
	err := runResolve(context.Background(), resolveOptions{
		HPIPath: hpiPath, PluginsPath: pluginsPath, Indent: 4, Fetch: ff.fetch,
	}, &buf)
	if err != nil {
		t.Fatalf("runResolve: %v", err)
	}
	if !strings.Contains(buf.String(), "artifactId: varroa-mite-auth") ||
		!strings.Contains(buf.String(), "version: \"1.0-SNAPSHOT\"") ||
		!strings.Contains(buf.String(), "artifactId: mailer") {
		t.Errorf("unexpected output:\n%s", buf.String())
	}
}
