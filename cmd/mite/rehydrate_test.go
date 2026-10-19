package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// gzipBytes gzips data for use as a RehydrationFile's gzip_content.
func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// rehydrationFile builds a RehydrationFile whose gzip_content and raw_size
// both describe data.
func rehydrationFile(t *testing.T, path string, data []byte) *mitev1.RehydrationFile {
	t.Helper()
	return &mitev1.RehydrationFile{
		Path:        path,
		GzipContent: gzipBytes(t, data),
		RawSize:     int64(len(data)),
	}
}

// --- 7.1: the path checker ---

func TestParseRehydrationPath_AcceptedShapes(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		wantKind     rehydrationPathKind
		wantJobPath  string
		wantBuild    int
		wantNodeName string
	}{
		{
			name:        "build.xml",
			path:        "jobs/team/jobs/pipeline/builds/7/build.xml",
			wantKind:    rehydrationPathBuildXML,
			wantJobPath: "team/pipeline",
			wantBuild:   7,
		},
		{
			name:        "changelog0.xml",
			path:        "jobs/team/jobs/pipeline/builds/7/changelog0.xml",
			wantKind:    rehydrationPathChangelog,
			wantJobPath: "team/pipeline",
			wantBuild:   7,
		},
		{
			name:        "nextBuildNumber",
			path:        "jobs/team/jobs/pipeline/nextBuildNumber",
			wantKind:    rehydrationPathNextBuildNumber,
			wantJobPath: "team/pipeline",
		},
		{
			name:        "program.dat",
			path:        "jobs/team/jobs/pipeline/builds/7/program.dat",
			wantKind:    rehydrationPathProgramDat,
			wantJobPath: "team/pipeline",
			wantBuild:   7,
		},
		{
			name:        "workflow node xml",
			path:        "jobs/team/jobs/pipeline/builds/7/workflow/3.xml",
			wantKind:    rehydrationPathWorkflowNode,
			wantJobPath: "team/pipeline",
			wantBuild:   7,
		},
		{
			name:         "node config.xml",
			path:         "nodes/agent-1/config.xml",
			wantKind:     rehydrationPathNodeConfig,
			wantNodeName: "agent-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRehydrationPath(tc.path)
			if err != nil {
				t.Fatalf("parseRehydrationPath(%q): %v", tc.path, err)
			}
			if got.kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", got.kind, tc.wantKind)
			}
			if got.jobPath != tc.wantJobPath {
				t.Errorf("jobPath = %q, want %q", got.jobPath, tc.wantJobPath)
			}
			if got.buildNumber != tc.wantBuild {
				t.Errorf("buildNumber = %d, want %d", got.buildNumber, tc.wantBuild)
			}
			if got.nodeName != tc.wantNodeName {
				t.Errorf("nodeName = %q, want %q", got.nodeName, tc.wantNodeName)
			}
		})
	}
}

func TestParseRehydrationPath_Rejections(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"empty segment", "jobs//nextBuildNumber"},
		{"dot segment", "jobs/./nextBuildNumber"},
		{"dotdot segment", "jobs/../nextBuildNumber"},
		{"leading dot segment", "jobs/.hidden/nextBuildNumber"},
		{"separator inside a segment", "jobs/foo\\bar/nextBuildNumber"},
		{"unrecognized shape", "jobs/team/config.xml"},
		{"leading zero build number", "jobs/team/builds/07/build.xml"},
		{"non-numeric build number", "jobs/team/builds/x/build.xml"},
		{"workflow node leading zero", "jobs/team/builds/7/workflow/01.xml"},
		{"node segment traverses", "nodes/../config.xml"},
		{"node segment hidden", "nodes/.hidden/config.xml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRehydrationPath(tc.path); err == nil {
				t.Errorf("parseRehydrationPath(%q): expected error, got none", tc.path)
			}
		})
	}
}

func TestConfineRehydrationPath_RefusesEscape(t *testing.T) {
	home := t.TempDir()
	if _, err := confineRehydrationPath(home, "../etc/passwd"); err == nil {
		t.Error("confineRehydrationPath: expected error for a path outside JENKINS_HOME, got none")
	}
}

func TestConfineRehydrationPath_AcceptsInsidePath(t *testing.T) {
	home := t.TempDir()
	got, err := confineRehydrationPath(home, "jobs/team/nextBuildNumber")
	if err != nil {
		t.Fatalf("confineRehydrationPath: %v", err)
	}
	want := filepath.Join(home, "jobs", "team", "nextBuildNumber")
	if got != want {
		t.Errorf("confineRehydrationPath = %q, want %q", got, want)
	}
}

func TestCheckRehydrationPath_RefusesSymlinkInParentChain(t *testing.T) {
	home := t.TempDir()
	realDir := filepath.Join(home, "real-jobs")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(home, "jobs")
	if err := os.Symlink(realDir, linkPath); err != nil {
		t.Fatal(err)
	}

	_, _, err := checkRehydrationPath(home, "jobs/team/nextBuildNumber")
	if err == nil {
		t.Error("checkRehydrationPath: expected error for a symbolic link in the parent chain, got none")
	}
}

func TestCheckRehydrationPath_TraversalPathFails(t *testing.T) {
	home := t.TempDir()
	if _, _, err := checkRehydrationPath(home, "jobs/../../etc/passwd"); err == nil {
		t.Error("checkRehydrationPath: expected error for a traversal path, got none")
	}
}

// --- 7.2: the writer ---

func TestAtomicWriteRehydrationFile_SetsFileMode(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "jobs", "team", "nextBuildNumber")
	if err := atomicWriteRehydrationFile(target, []byte("5\n")); err != nil {
		t.Fatalf("atomicWriteRehydrationFile: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Errorf("file mode = %o, want 0644", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatalf("stat parent directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0755 {
		t.Errorf("directory mode = %o, want 0755", got)
	}
}

func TestInflateRehydrationContent_SizeMismatchFails(t *testing.T) {
	data := []byte("hello, build record")
	gz := gzipBytes(t, data)
	if _, err := inflateRehydrationContent(gz, int64(len(data))-1); err == nil {
		t.Error("inflateRehydrationContent: expected error for a raw_size mismatch, got none")
	}
}

func TestInflateRehydrationContent_MatchingSizeSucceeds(t *testing.T) {
	data := []byte("hello, build record")
	gz := gzipBytes(t, data)
	got, err := inflateRehydrationContent(gz, int64(len(data)))
	if err != nil {
		t.Fatalf("inflateRehydrationContent: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("inflated content = %q, want %q", got, data)
	}
}

// --- 7.3: the three write rules, and the write order ---

// seedJob creates a job directory with a config.xml under home, so the
// job "exists" for the mite's defer check.
func seedJob(t *testing.T, home string, jobDirParts ...string) string {
	t.Helper()
	dir := filepath.Join(append([]string{home}, jobDirParts...)...)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.xml"), []byte("<flow-definition/>"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApplyRehydrationBundle_SkipsExistingBuildDirectory(t *testing.T) {
	home := t.TempDir()
	jobDir := seedJob(t, home, "jobs", "team")
	buildDir := filepath.Join(jobDir, "builds", "7")
	if err := os.MkdirAll(buildDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(buildDir, "build.xml"), []byte("<original/>"), 0644); err != nil {
		t.Fatal(err)
	}

	bundle := &mitev1.RehydrationBundle{
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/team/builds/7/build.xml", []byte("<seeded/>")),
		},
	}
	if _, err := applyRehydrationBundle(home, bundle); err != nil {
		t.Fatalf("applyRehydrationBundle: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(buildDir, "build.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "<original/>" {
		t.Errorf("existing build.xml was overwritten: got %q", got)
	}
}

func TestApplyRehydrationBundle_DefersMissingJob(t *testing.T) {
	home := t.TempDir()

	bundle := &mitev1.RehydrationBundle{
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/team/jobs/branch-not-indexed-yet/builds/1/build.xml", []byte("<seeded/>")),
		},
	}
	result, err := applyRehydrationBundle(home, bundle)
	if err != nil {
		t.Fatalf("applyRehydrationBundle: %v", err)
	}
	if len(result.deferredJobs) != 1 || result.deferredJobs[0] != "team/branch-not-indexed-yet" {
		t.Errorf("deferredJobs = %v, want [team/branch-not-indexed-yet]", result.deferredJobs)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "team", "jobs", "branch-not-indexed-yet", "builds", "1", "build.xml")); !os.IsNotExist(err) {
		t.Errorf("expected no build.xml written for a deferred job, stat error = %v", err)
	}
}

func TestApplyRehydrationBundle_NeverLowersNextBuildNumber(t *testing.T) {
	home := t.TempDir()
	jobDir := seedJob(t, home, "jobs", "team")
	if err := os.WriteFile(filepath.Join(jobDir, "nextBuildNumber"), []byte("40"), 0644); err != nil {
		t.Fatal(err)
	}

	bundle := &mitev1.RehydrationBundle{
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/team/nextBuildNumber", []byte("12")),
		},
	}
	if _, err := applyRehydrationBundle(home, bundle); err != nil {
		t.Fatalf("applyRehydrationBundle: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(jobDir, "nextBuildNumber"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "40" {
		t.Errorf("nextBuildNumber = %q, want 40 (never lowered)", got)
	}
}

func TestApplyRehydrationBundle_RaisesNextBuildNumberWhenHigher(t *testing.T) {
	home := t.TempDir()
	jobDir := seedJob(t, home, "jobs", "team")
	if err := os.WriteFile(filepath.Join(jobDir, "nextBuildNumber"), []byte("5"), 0644); err != nil {
		t.Fatal(err)
	}

	bundle := &mitev1.RehydrationBundle{
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/team/nextBuildNumber", []byte("12")),
		},
	}
	if _, err := applyRehydrationBundle(home, bundle); err != nil {
		t.Fatalf("applyRehydrationBundle: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(jobDir, "nextBuildNumber"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "12" {
		t.Errorf("nextBuildNumber = %q, want 12", got)
	}
}

// TestApplyRehydrationBundle_WritesChangelogBeforeBuildXML proves the write
// order regardless of the bundle's own file order: it lists build.xml
// before changelog0.xml, but gives build.xml broken gzip content so its
// write fails. If the changelog was written first, as the write rules
// require, it survives on disk even though the whole section then reports
// an error for the broken build.xml.
func TestApplyRehydrationBundle_WritesChangelogBeforeBuildXML(t *testing.T) {
	home := t.TempDir()
	jobDir := seedJob(t, home, "jobs", "team")

	brokenBuildXML := &mitev1.RehydrationFile{
		Path:        "jobs/team/builds/3/build.xml",
		GzipContent: []byte("not gzip data"),
		RawSize:     9,
	}
	changelog := rehydrationFile(t, "jobs/team/builds/3/changelog0.xml", []byte("<varroa-changelog/>"))

	bundle := &mitev1.RehydrationBundle{
		Files: []*mitev1.RehydrationFile{brokenBuildXML, changelog},
	}
	if _, err := applyRehydrationBundle(home, bundle); err == nil {
		t.Fatal("applyRehydrationBundle: expected an error from the broken build.xml content")
	}

	buildDir := filepath.Join(jobDir, "builds", "3")
	if _, err := os.Stat(filepath.Join(buildDir, "changelog0.xml")); err != nil {
		t.Errorf("changelog0.xml was not written before build.xml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(buildDir, "build.xml")); !os.IsNotExist(err) {
		t.Errorf("build.xml should not exist after its write failed, stat error = %v", err)
	}
}

// TestApplyRehydrationBundle_SecondApplyIsFullyIdempotent proves the three
// write rules combine into full idempotency: applying the same bundle a
// second time rewrites no file, leaves nextBuildNumber unchanged, and
// reports the same outcome as the first apply. It back-dates every written
// file's mtime after the first apply, because a write-then-rename always
// produces a fresh mtime, so an unchanged mtime after the second apply
// proves the file was not touched.
func TestApplyRehydrationBundle_SecondApplyIsFullyIdempotent(t *testing.T) {
	home := t.TempDir()
	jobDir := seedJob(t, home, "jobs", "team")

	bundle := &mitev1.RehydrationBundle{
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/team/builds/5/build.xml", []byte("<flow-build/>")),
			rehydrationFile(t, "jobs/team/builds/5/changelog0.xml", []byte("<varroa-changelog/>")),
			rehydrationFile(t, "jobs/team/nextBuildNumber", []byte("6")),
		},
	}

	firstResult, err := applyRehydrationBundle(home, bundle)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}

	buildDir := filepath.Join(jobDir, "builds", "5")
	paths := []string{
		filepath.Join(buildDir, "build.xml"),
		filepath.Join(buildDir, "changelog0.xml"),
		filepath.Join(jobDir, "nextBuildNumber"),
	}

	backdated := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	beforeContent := make(map[string][]byte, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s after first apply: %v", p, err)
		}
		beforeContent[p] = data
		if err := os.Chtimes(p, backdated, backdated); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}

	secondResult, err := applyRehydrationBundle(home, bundle)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(firstResult.deferredJobs) != len(secondResult.deferredJobs) {
		t.Errorf("outcome changed between applies: first deferredJobs=%v, second=%v", firstResult.deferredJobs, secondResult.deferredJobs)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s after second apply: %v", p, err)
		}
		if !info.ModTime().Equal(backdated) {
			t.Errorf("file %s was rewritten on the second apply: mtime = %v, want unchanged %v", p, info.ModTime(), backdated)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s after second apply: %v", p, err)
		}
		if !bytes.Equal(data, beforeContent[p]) {
			t.Errorf("file %s content changed on the second apply", p)
		}
	}

	got, err := os.ReadFile(filepath.Join(jobDir, "nextBuildNumber"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "6" {
		t.Errorf("nextBuildNumber = %q, want unchanged 6", got)
	}
}

func TestApplyRehydrationBundle_TraversalPathFailsSection(t *testing.T) {
	home := t.TempDir()
	bundle := &mitev1.RehydrationBundle{
		Files: []*mitev1.RehydrationFile{
			rehydrationFile(t, "jobs/../../etc/passwd", []byte("evil")),
		},
	}
	if _, err := applyRehydrationBundle(home, bundle); err == nil {
		t.Error("applyRehydrationBundle: expected an error for a traversal path")
	}
}

// --- branch and mangled-name resolution ---

const (
	branchJobConfig = `<flow-definition plugin="workflow-job"><properties>` +
		`<org.jenkinsci.plugins.workflow.multibranch.BranchJobProperty plugin="workflow-multibranch">` +
		`<branch class="jenkins.branch.Branch"><head class="jenkins.plugins.git.GitBranchSCMHead">` +
		`<name>%s</name></head></branch></org.jenkinsci.plugins.workflow.multibranch.BranchJobProperty>` +
		`</properties></flow-definition>`
	repoProjectConfig = `<org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject plugin="workflow-multibranch">` +
		`<properties><jenkins.branch.ProjectNameProperty><name>%s</name></jenkins.branch.ProjectNameProperty></properties>` +
		`</org.jenkinsci.plugins.workflow.multibranch.WorkflowMultiBranchProject>`
)

// writeConfigAt writes config under dir, creating dir first.
func writeConfigAt(t *testing.T, dir, config string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.xml"), []byte(jenkinsXMLProlog+config), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestJenkinsXMLReader checks the declaration rewrite. A stream without a
// version 1.1 declaration must pass through unchanged.
func TestJenkinsXMLReader(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"jenkins single quotes", "<?xml version='1.1' encoding='UTF-8'?>\n<hudson/>", "<?xml version='1.0' encoding='UTF-8'?>\n<hudson/>"},
		{"double quotes", `<?xml version="1.1"?><hudson/>`, `<?xml version="1.0"?><hudson/>`},
		{"white space around equals", "<?xml version = '1.1' encoding='UTF-8'?><hudson/>", "<?xml version = '1.0' encoding='UTF-8'?><hudson/>"},
		{"version 1.0 unchanged", "<?xml version='1.0' encoding='UTF-8'?>\n<hudson/>", "<?xml version='1.0' encoding='UTF-8'?>\n<hudson/>"},
		{"long declaration", "<?xml version='1.1'" + strings.Repeat(" ", 200) + "encoding='UTF-8'?><hudson/>", "<?xml version='1.0'" + strings.Repeat(" ", 200) + "encoding='UTF-8'?><hudson/>"},
		{"no declaration", "<hudson/>", "<hudson/>"},
		{"empty stream", "", ""},
		{"declaration without end", "<?xml version='1.1'", "<?xml version='1.1'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := io.ReadAll(jenkinsXMLReader(strings.NewReader(tc.in)))
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("output = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRootElementName_JenkinsVersion11 reads a root element from a file
// that carries the version 1.1 declaration Jenkins writes.
func TestRootElementName_JenkinsVersion11(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.xml")
	body := jenkinsXMLProlog + `<jenkins.branch.OrganizationFolder plugin="branch-api@2.1280.v0d4e5b_b_460ef">` +
		`</jenkins.branch.OrganizationFolder>`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := rootElementName(path)
	if err != nil {
		t.Fatalf("rootElementName: %v", err)
	}
	if want := "jenkins.branch.OrganizationFolder"; got != want {
		t.Errorf("rootElementName = %q, want %q", got, want)
	}
}

// TestItemNameFromConfig_JenkinsVersion11 reads a branch name and a
// repository name from files that carry the version 1.1 declaration.
func TestItemNameFromConfig_JenkinsVersion11(t *testing.T) {
	dir := t.TempDir()
	branchPath := filepath.Join(dir, "branch.xml")
	if err := os.WriteFile(branchPath, []byte(jenkinsXMLProlog+fmt.Sprintf(branchJobConfig, "main")), 0644); err != nil {
		t.Fatal(err)
	}
	name, ok, err := itemNameFromConfig(branchPath, "branches")
	if err != nil || !ok || name != "main" {
		t.Errorf("branch: itemNameFromConfig = (%q, %v, %v), want (main, true, nil)", name, ok, err)
	}

	repoPath := filepath.Join(dir, "repo.xml")
	if err := os.WriteFile(repoPath, []byte(jenkinsXMLProlog+fmt.Sprintf(repoProjectConfig, "projgen")), 0644); err != nil {
		t.Fatal(err)
	}
	name, ok, err = itemNameFromConfig(repoPath, "jobs")
	if err != nil || !ok || name != "projgen" {
		t.Errorf("repo: itemNameFromConfig = (%q, %v, %v), want (projgen, true, nil)", name, ok, err)
	}
}

func TestDecodeItemName(t *testing.T) {
	cases := map[string]string{
		"main":         "main",
		"feature%2Fx":  "feature/x",
		"feature%2fx":  "feature/x",
		"a%25b":        "a%b",
		"a%23b%3Fc":    "a#b?c",
		"a%3fb%2fc":    "a?b/c",
		"%5Bx%5D%5Cy":  `[x]\y`,
		"%2E":          ".",
		"%2E.":         "..",
		"%00":          "",
		"%20kept":      "%20kept",
		"no-escapes.x": "no-escapes.x",
	}
	for seg, want := range cases {
		if got := decodeItemName(seg); got != want {
			t.Errorf("decodeItemName(%q) = %q, want %q", seg, got, want)
		}
	}
}

func TestResolveJobDir_MultibranchBranch(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	writeConfigAt(t, filepath.Join(home, "jobs", "mb", "branches", "main"), fmt.Sprintf(branchJobConfig, "main"))

	parts, ok := newJobDirResolver(home).resolve("mb/main")
	if !ok {
		t.Fatal("resolve(mb/main) = false, want true")
	}
	if want := []string{"jobs", "mb", "branches", "main"}; !reflect.DeepEqual(parts, want) {
		t.Errorf("parts = %v, want %v", parts, want)
	}
}

func TestResolveJobDir_EncodedBranchName(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	writeConfigAt(t, filepath.Join(home, "jobs", "mb", "branches", "feature-x.abc123"), fmt.Sprintf(branchJobConfig, "feature/x"))

	parts, ok := newJobDirResolver(home).resolve("mb/feature%2Fx")
	if !ok {
		t.Fatal("resolve(mb/feature%2Fx) = false, want true")
	}
	if want := []string{"jobs", "mb", "branches", "feature-x.abc123"}; !reflect.DeepEqual(parts, want) {
		t.Errorf("parts = %v, want %v", parts, want)
	}
}

func TestResolveJobDir_OrganizationFolderMangledRepo(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "org", orgFolderConfig)
	repoDir := filepath.Join(home, "jobs", "org", "jobs", "my-repo.k3j2a1")
	writeConfigAt(t, repoDir, fmt.Sprintf(repoProjectConfig, "my.repo"))
	writeConfigAt(t, filepath.Join(repoDir, "branches", "main"), fmt.Sprintf(branchJobConfig, "main"))

	parts, ok := newJobDirResolver(home).resolve("org/my.repo/main")
	if !ok {
		t.Fatal("resolve(org/my.repo/main) = false, want true")
	}
	want := []string{"jobs", "org", "jobs", "my-repo.k3j2a1", "branches", "main"}
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("parts = %v, want %v", parts, want)
	}
}

func TestResolveJobDir_UnindexedBranchAnswersFalse(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)

	if _, ok := newJobDirResolver(home).resolve("mb/main"); ok {
		t.Error("resolve(mb/main) = true with no branch directory, want false")
	}
}

func TestResolveJobDir_SkipsSymlinkedBranchDir(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	target := t.TempDir()
	writeConfigAt(t, target, fmt.Sprintf(branchJobConfig, "main"))
	branches := filepath.Join(home, "jobs", "mb", "branches")
	if err := os.MkdirAll(branches, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(branches, "main")); err != nil {
		t.Fatal(err)
	}

	if _, ok := newJobDirResolver(home).resolve("mb/main"); ok {
		t.Error("resolve(mb/main) = true through a symbolic link, want false")
	}
}

func TestApplyRehydrationBundle_SeedsBranchJob(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	branchDir := filepath.Join(home, "jobs", "mb", "branches", "main")
	writeConfigAt(t, branchDir, fmt.Sprintf(branchJobConfig, "main"))

	bundle := &mitev1.RehydrationBundle{Files: []*mitev1.RehydrationFile{
		rehydrationFile(t, "jobs/mb/jobs/main/nextBuildNumber", []byte("4")),
		rehydrationFile(t, "jobs/mb/jobs/main/builds/3/build.xml", []byte("<build/>")),
	}}
	res, err := applyRehydrationBundle(home, bundle)
	if err != nil {
		t.Fatalf("applyRehydrationBundle: %v", err)
	}
	if len(res.deferredJobs) != 0 {
		t.Errorf("deferredJobs = %v, want none", res.deferredJobs)
	}
	if _, err := os.Stat(filepath.Join(branchDir, "builds", "3", "build.xml")); err != nil {
		t.Errorf("build.xml not written under branches/main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(branchDir, "nextBuildNumber")); err != nil {
		t.Errorf("nextBuildNumber not written under branches/main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "mb", "jobs")); !os.IsNotExist(err) {
		t.Errorf("a logical jobs/mb/jobs directory was created, stat err = %v", err)
	}
}

func TestApplyRehydrationBundle_RebasesChangelogFileOfBranchJob(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	branchDir := filepath.Join(home, "jobs", "mb", "branches", "main")
	writeConfigAt(t, branchDir, fmt.Sprintf(branchJobConfig, "main"))

	buildXML := "<build><checkouts><checkout><changelogFile>" +
		home + "/jobs/mb/jobs/main/builds/3/changelog0.xml" +
		"</changelogFile></checkout></checkouts></build>"
	bundle := &mitev1.RehydrationBundle{Files: []*mitev1.RehydrationFile{
		rehydrationFile(t, "jobs/mb/jobs/main/builds/3/build.xml", []byte(buildXML)),
		rehydrationFile(t, "jobs/mb/jobs/main/builds/3/changelog0.xml", []byte("<varroa-changelog/>")),
	}}
	if _, err := applyRehydrationBundle(home, bundle); err != nil {
		t.Fatalf("applyRehydrationBundle: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(branchDir, "builds", "3", "build.xml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "<build><checkouts><checkout><changelogFile>" +
		home + "/jobs/mb/branches/main/builds/3/changelog0.xml" +
		"</changelogFile></checkout></checkouts></build>"
	if string(got) != want {
		t.Errorf("build.xml = %q, want %q", got, want)
	}
}

func TestApplyRehydrationBundle_LogicalJobKeepsChangelogFile(t *testing.T) {
	home := t.TempDir()
	jobDir := seedJob(t, home, "jobs", "team", "jobs", "pipeline")
	buildXML := "<build><checkouts><checkout><changelogFile>" +
		home + "/jobs/team/jobs/pipeline/builds/3/changelog0.xml" +
		"</changelogFile></checkout></checkouts></build>"
	bundle := &mitev1.RehydrationBundle{Files: []*mitev1.RehydrationFile{
		rehydrationFile(t, "jobs/team/jobs/pipeline/builds/3/build.xml", []byte(buildXML)),
	}}
	if _, err := applyRehydrationBundle(home, bundle); err != nil {
		t.Fatalf("applyRehydrationBundle: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(jobDir, "builds", "3", "build.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != buildXML {
		t.Errorf("build.xml = %q, want the operator's content unchanged", got)
	}
}

func TestApplyRehydrationBundle_RefusesSymlinkedBuildsOfMovedJob(t *testing.T) {
	home := t.TempDir()
	writeJobConfig(t, home, "mb", multibranchConfig)
	branchDir := filepath.Join(home, "jobs", "mb", "branches", "main")
	writeConfigAt(t, branchDir, fmt.Sprintf(branchJobConfig, "main"))
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(branchDir, "builds")); err != nil {
		t.Fatal(err)
	}

	bundle := &mitev1.RehydrationBundle{Files: []*mitev1.RehydrationFile{
		rehydrationFile(t, "jobs/mb/jobs/main/builds/3/build.xml", []byte("<build/>")),
	}}
	if _, err := applyRehydrationBundle(home, bundle); err == nil {
		t.Fatal("applyRehydrationBundle: expected a refusal of the symbolic link")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("a file was written outside JENKINS_HOME: %v", entries)
	}
}
