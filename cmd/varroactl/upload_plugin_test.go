package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

// uploadFixture writes a stand-in .hpi. The CLI never parses it — the update
// center does — so any bytes will do.
func uploadFixture(t *testing.T) string {
	t.Helper()
	return writeHPIFixture(t, t.TempDir(), "varroa-mcp-tools.hpi", []byte("fake-hpi-bytes"))
}

const uploadSuccessBody = `{"plugin":{"name":"varroa-mcp-tools","version":"1.0.0","sha256":"sha256:abc",` +
	`"requiredCore":"2.492"},"dryRun":false,"packRef":"upload-9f2a1c0b3d4e","closure":[` +
	`{"name":"workflow-api","min":"1384.vdc05a_48f535f","status":"satisfied-store",` +
	`"resolvedVersion":"1413.v2ff1a_5e720fa_","source":"store"},` +
	`{"name":"some-lib","min":"1.2","status":"planned-fetch","resolvedVersion":"1.9",` +
	`"source":"upstream","fetched":true}],` +
	`"optionalDependencies":[{"name":"junit","min":"1.0"}],` +
	`"warnings":[{"code":"lock-too-old","plugin":"credentials","min":"1400.v0","message":"older pin"}]}`

const uploadRejectionBody = `{"error":"unresolved-dependencies","message":"1 of 2 mandatory dependencies could not be resolved",` +
	`"unresolved":[{"name":"old-thing","min":"9.0","reason":"unreachable","foundInStore":null,` +
	`"foundDeclared":null,"foundUpstream":"3.1","remediation":"upstream's newest version 3.1 is older than 9.0"}]}`

func TestUploadPlugin_Registered(t *testing.T) {
	root := newRootCmd()
	uploadCmd := lookupCommand(root, "upload")
	if uploadCmd == nil {
		t.Fatal("upload verb parent not registered")
	}
	if lookupCommand(uploadCmd, "plugin") == nil {
		t.Fatal("upload plugin noun not registered")
	}
}

func TestUploadPlugin_Success(t *testing.T) {
	testSetup(t)

	var (
		gotPath, gotQuery, gotAuth string
		gotFile                    []byte
		gotFilename                string
	)
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")

		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		part, err := mr.NextPart()
		if err != nil {
			t.Errorf("next part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotFilename = part.FileName()
		gotFile, _ = io.ReadAll(part)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(uploadSuccessBody))
	})
	defer srv.Close()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"upload", "plugin", uploadFixture(t)})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (out %s)", err, out.String())
	}

	if gotPath != "/api/v1/updatecenter/plugins" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query = %q, want empty without --dry-run", gotQuery)
	}
	if gotAuth != "Bearer vk_test_key" {
		t.Errorf("authorization = %q — the upload must use the user's API key, not a shared UC token", gotAuth)
	}
	if string(gotFile) != "fake-hpi-bytes" {
		t.Errorf("file bytes = %q", gotFile)
	}
	if gotFilename != "varroa-mcp-tools.hpi" {
		t.Errorf("filename = %q", gotFilename)
	}

	text := out.String()
	for _, want := range []string{
		"Uploaded varroa-mcp-tools@1.0.0",
		"upload-9f2a1c0b3d4e",
		"workflow-api",
		"satisfied-store",
		"1 downloaded",
		"junit >= 1.0",
		"lock-too-old",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output is missing %q:\n%s", want, text)
		}
	}
}

func TestUploadPlugin_DryRun(t *testing.T) {
	testSetup(t)

	var gotQuery string
	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Replace(uploadSuccessBody, `"dryRun":false`, `"dryRun":true`, 1)))
	})
	defer srv.Close()

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"upload", "plugin", uploadFixture(t), "--dry-run"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (out %s)", err, out.String())
	}
	if gotQuery != "dryRun=true" {
		t.Errorf("query = %q, want dryRun=true", gotQuery)
	}
	if !strings.Contains(out.String(), "Previewed varroa-mcp-tools@1.0.0") {
		t.Errorf("output = %s", out.String())
	}
	if !strings.Contains(out.String(), "would be downloaded") {
		t.Errorf("a dry run must not claim anything was downloaded:\n%s", out.String())
	}
}

func TestUploadPlugin_JSONOutputIsVerbatim(t *testing.T) {
	testSetup(t)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(uploadSuccessBody))
	})
	defer srv.Close()

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"upload", "plugin", uploadFixture(t), "-o", "json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.TrimSpace(out.String()) != uploadSuccessBody {
		t.Fatalf("-o json must print the envelope verbatim:\n got %s\nwant %s", out.String(), uploadSuccessBody)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}

func TestUploadPlugin_RejectionExitsNonZero(t *testing.T) {
	testSetup(t)

	srv := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(uploadRejectionBody))
	})
	defer srv.Close()

	var out, errOut bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs([]string{"upload", "plugin", uploadFixture(t)})
	err := root.Execute()
	if err == nil {
		t.Fatal("a 422 must exit non-zero")
	}
	if !strings.Contains(err.Error(), "unresolved-dependencies") {
		t.Errorf("error = %v", err)
	}
	// The actionable table goes to stderr.
	text := errOut.String()
	for _, want := range []string{"old-thing", "unreachable", "3.1", "upstream's newest version"} {
		if !strings.Contains(text, want) {
			t.Errorf("stderr is missing %q:\n%s", want, text)
		}
	}
	if out.Len() != 0 {
		t.Errorf("a rejection must not write to stdout: %s", out.String())
	}
}

// TestUploadPlugin_FailsBeforeAnyNetworkCall: with no resolvable context the
// command must fail without opening a connection.
func TestUploadPlugin_NoContext(t *testing.T) {
	testSetup(t) // clears VARROACTL_SERVER / VARROACTL_API_KEY

	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"upload", "plugin", uploadFixture(t)})
	if err := root.Execute(); err == nil {
		t.Fatal("expected an error with no resolved context")
	}
}
