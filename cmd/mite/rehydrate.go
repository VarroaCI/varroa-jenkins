// rehydrate.go applies a RehydrationBundle under JENKINS_HOME. It writes
// only allow-listed paths, defers a job that does not exist yet, never
// overwrites a real build, and never lowers a stored nextBuildNumber. See
// design D5 and the build-record-rehydration spec.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// rehydrationPathKind identifies which of the six accepted file shapes a
// rehydration path matches.
type rehydrationPathKind int

const (
	rehydrationPathBuildXML rehydrationPathKind = iota
	rehydrationPathChangelog
	rehydrationPathNextBuildNumber
	// The pipeline mesh adds three shapes. A resume writes them; a
	// desired-state rehydration bundle never sends one.
	rehydrationPathProgramDat
	rehydrationPathWorkflowNode
	rehydrationPathNodeConfig
)

// The file names the "builds" shape accepts, and the two fixed names of
// the mesh's own shapes.
const (
	changelogFileName  = "changelog0.xml"
	buildFileName      = "build.xml"
	programDatFileName = "program.dat"
	workflowDirName    = "workflow"
	nodesDirName       = "nodes"
	nodeConfigFileName = "config.xml"
)

// parsedRehydrationPath is one validated rehydration file path.
type parsedRehydrationPath struct {
	kind rehydrationPathKind
	// jobPath is the job's human path, "/"-joined ("team/pipeline").
	jobPath string
	// jobDirParts are the filesystem path parts under jenkinsHomeDir that
	// name the job's own directory ("jobs", "team", "jobs", "pipeline").
	jobDirParts []string
	// buildNumber is set only for a shape that names a build.
	buildNumber int
	// nodeName is set only for rehydrationPathNodeConfig.
	nodeName string
}

// validateJobSegment checks one job-path segment against the allow-list: it
// must not be empty, must not start with a dot (so it can never be "." or
// ".."), and must hold no path separator.
func validateJobSegment(seg string) error {
	if seg == "" {
		return errors.New("empty path segment")
	}
	if strings.HasPrefix(seg, ".") {
		return fmt.Errorf("path segment %q starts with a dot", seg)
	}
	if strings.ContainsAny(seg, "/\\") {
		return fmt.Errorf("path segment %q holds a path separator", seg)
	}
	return nil
}

// parseBuildNumber checks a build-number segment against [1-9][0-9]*: no
// leading zero, no sign, digits only.
func parseBuildNumber(s string) (int, error) {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return 0, fmt.Errorf("invalid build number %q", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid build number %q", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid build number %q: %w", s, err)
	}
	return n, nil
}

// parseRehydrationPath checks relPath against the six accepted shapes:
//
//	jobs/<seg>(/jobs/<seg>)*/builds/<n>/build.xml
//	jobs/<seg>(/jobs/<seg>)*/builds/<n>/changelog0.xml
//	jobs/<seg>(/jobs/<seg>)*/builds/<n>/program.dat
//	jobs/<seg>(/jobs/<seg>)*/builds/<n>/workflow/<m>.xml
//	jobs/<seg>(/jobs/<seg>)*/nextBuildNumber
//	nodes/<seg>/config.xml
//
// Any other shape fails. relPath uses forward slashes, matching the wire
// format the operator sends.
func parseRehydrationPath(relPath string) (parsedRehydrationPath, error) {
	if relPath == "" {
		return parsedRehydrationPath{}, errors.New("rehydration path is empty")
	}
	parts := strings.Split(relPath, "/")

	if parts[0] == nodesDirName {
		return parseNodeConfigPath(relPath, parts)
	}

	var jobSegs, dirParts []string
	i := 0
	for {
		if i >= len(parts) || parts[i] != "jobs" {
			return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: does not match an accepted shape", relPath)
		}
		dirParts = append(dirParts, "jobs")
		i++
		if i >= len(parts) {
			return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: missing job segment", relPath)
		}
		seg := parts[i]
		if err := validateJobSegment(seg); err != nil {
			return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: %w", relPath, err)
		}
		dirParts = append(dirParts, seg)
		jobSegs = append(jobSegs, seg)
		i++
		if i < len(parts) && parts[i] == "jobs" {
			continue
		}
		break
	}

	rest := parts[i:]
	parsed := parsedRehydrationPath{jobPath: strings.Join(jobSegs, "/"), jobDirParts: dirParts}

	switch {
	case len(rest) == 1 && rest[0] == "nextBuildNumber":
		parsed.kind = rehydrationPathNextBuildNumber
		return parsed, nil
	case len(rest) == 3 && rest[0] == "builds" && buildFileKind(rest[2]) >= 0:
		n, err := parseBuildNumber(rest[1])
		if err != nil {
			return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: %w", relPath, err)
		}
		parsed.buildNumber = n
		parsed.kind = rehydrationPathKind(buildFileKind(rest[2]))
		return parsed, nil
	case len(rest) == 4 && rest[0] == "builds" && rest[2] == workflowDirName && strings.HasSuffix(rest[3], ".xml"):
		n, err := parseBuildNumber(rest[1])
		if err != nil {
			return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: %w", relPath, err)
		}
		// A workflow node number takes the same digits rule a build
		// number takes, so no leading zero and no sign can pass.
		if _, err := parseBuildNumber(strings.TrimSuffix(rest[3], ".xml")); err != nil {
			return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: %w", relPath, err)
		}
		parsed.buildNumber = n
		parsed.kind = rehydrationPathWorkflowNode
		return parsed, nil
	default:
		return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: does not match an accepted shape", relPath)
	}
}

// buildFileKind maps a build-directory file name onto its path kind, or
// -1 for a name outside the accepted set.
func buildFileKind(name string) int {
	switch name {
	case buildFileName:
		return int(rehydrationPathBuildXML)
	case changelogFileName:
		return int(rehydrationPathChangelog)
	case programDatFileName:
		return int(rehydrationPathProgramDat)
	default:
		return -1
	}
}

// parseNodeConfigPath checks the nodes/<seg>/config.xml shape. The node
// segment takes the same allow-list a job segment takes, so a node
// named ".." is refused.
func parseNodeConfigPath(relPath string, parts []string) (parsedRehydrationPath, error) {
	if len(parts) != 3 || parts[2] != nodeConfigFileName {
		return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: does not match an accepted shape", relPath)
	}
	if err := validateJobSegment(parts[1]); err != nil {
		return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: %w", relPath, err)
	}
	return parsedRehydrationPath{kind: rehydrationPathNodeConfig, nodeName: parts[1]}, nil
}

// confineRehydrationPath joins relPath under jenkinsHomeDir and refuses a
// result that resolves outside jenkinsHomeDir.
func confineRehydrationPath(jenkinsHomeDir, relPath string) (string, error) {
	if relPath == "" {
		return "", errors.New("rehydration path is empty")
	}
	home := filepath.Clean(jenkinsHomeDir)
	abs := filepath.Clean(filepath.Join(home, relPath))
	if abs != home && !strings.HasPrefix(abs, home+string(filepath.Separator)) {
		return "", fmt.Errorf("rehydration path %q resolves outside JENKINS_HOME", relPath)
	}
	return abs, nil
}

// checkNoSymlinkInParents refuses a path whose parent chain, from root down
// to the immediate parent of absPath, holds a symbolic link. A path
// component that does not exist yet is not checked further: the mite
// creates it as a real directory.
func checkNoSymlinkInParents(root, absPath string) error {
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return fmt.Errorf("resolve %q relative to %q: %w", absPath, root, err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for _, p := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, p)
		info, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("stat %q: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is a symbolic link", cur)
		}
	}
	return nil
}

// checkRehydrationPath is the path checker: it validates one
// RehydrationFile.Path against the allow-listed shapes, confines it under
// jenkinsHomeDir, and refuses a symbolic link in its parent chain. It
// returns the parsed path and the absolute filesystem path the mite may
// write to.
func checkRehydrationPath(jenkinsHomeDir, relPath string) (parsedRehydrationPath, string, error) {
	parsed, err := parseRehydrationPath(relPath)
	if err != nil {
		return parsedRehydrationPath{}, "", err
	}
	absPath, err := confineRehydrationPath(jenkinsHomeDir, relPath)
	if err != nil {
		return parsedRehydrationPath{}, "", err
	}
	if err := checkNoSymlinkInParents(jenkinsHomeDir, absPath); err != nil {
		return parsedRehydrationPath{}, "", err
	}
	return parsed, absPath, nil
}

// inflateRehydrationContent decompresses gzipContent with a reader bounded
// to rawSize+1 bytes, so a mismatched declared size can never force an
// unbounded inflation. It fails when the inflated length differs from
// rawSize.
func inflateRehydrationContent(gzipContent []byte, rawSize int64) ([]byte, error) {
	if rawSize < 0 {
		return nil, fmt.Errorf("invalid raw_size %d", rawSize)
	}
	gz, err := gzip.NewReader(bytes.NewReader(gzipContent))
	if err != nil {
		return nil, fmt.Errorf("open gzip content: %w", err)
	}
	defer func() { _ = gz.Close() }()

	data, err := io.ReadAll(io.LimitReader(gz, rawSize+1))
	if err != nil {
		return nil, fmt.Errorf("inflate content: %w", err)
	}
	if int64(len(data)) != rawSize {
		return nil, fmt.Errorf("inflated size %d does not match declared raw_size %d", len(data), rawSize)
	}
	return data, nil
}

// atomicWriteRehydrationFile writes data to absPath with mode 0644, using a
// temporary name and a rename so a reader never observes a partial file. It
// creates the parent directory with mode 0755 first.
func atomicWriteRehydrationFile(absPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
		return fmt.Errorf("create directory for %s: %w", absPath, err)
	}
	tmp := absPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write temp file for %s: %w", absPath, err)
	}
	if err := os.Rename(tmp, absPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp file for %s: %w", absPath, err)
	}
	return nil
}

// errRehydrationFileExists reports a refused overwrite of an existing
// file. A resume command treats an existing node config as an expected
// outcome, not as a command failure.
var errRehydrationFileExists = errors.New("rehydration file already exists")

// writeRehydrationFileAt writes data at absPath. allowOverwrite lets the
// write land on a path that already holds a file. Only the resume path
// sets the flag: applyRehydrationBundle keeps its own rules, which skip
// an existing build directory before any write.
func writeRehydrationFileAt(absPath string, data []byte, allowOverwrite bool) error {
	if !allowOverwrite {
		if _, err := os.Stat(absPath); err == nil {
			return fmt.Errorf("file %s already exists: %w", absPath, errRehydrationFileExists)
		}
	}
	return atomicWriteRehydrationFile(absPath, data)
}

// jobConfigExists reports whether the job at jobDir already holds a
// config.xml. A job whose config.xml is absent is not created yet, so its
// records must defer rather than write an orphaned directory.
func jobConfigExists(jobDir string) bool {
	_, err := os.Stat(filepath.Join(jobDir, "config.xml"))
	return err == nil
}

// buildDirExists reports whether jobDir already holds a build directory for
// buildNumber. An existing build directory holds real history, seeded or
// not, and the mite never overwrites it.
func buildDirExists(buildDir string) bool {
	info, err := os.Stat(buildDir)
	return err == nil && info.IsDir()
}

// branchIndexSucceeded is the index result of a branch scan that finished
// without error. The plugin reports it on a multibranch project row.
const branchIndexSucceeded = "SUCCESS"

// jobCreator answers which ancestor of a job path can still create the job.
type jobCreator struct {
	// canCreate has the meaning of the old parentCanCreateJob result.
	canCreate bool
	// folder is the job path of the multibranch project or organization
	// folder that decided. It is "" when no such folder decided.
	folder string
}

// orphanDeferredJobs splits deferred job paths after a successful items
// apply. A job whose parent is a multibranch project or an organization
// folder stays deferred while branch indexing can still create it. A job
// under such a folder orphans once the polled branch inventory shows a
// completed, successful scan of the folder that does not list the job. The
// loader returns the inventory and runs at most once. A nil loader, or a nil
// set, keeps the job. A job whose parent directory exists without a
// config.xml also stays deferred. Any other job the items apply did not
// create no longer exists, so its records have nothing to seed.
func orphanDeferredJobs(jenkinsHome string, deferred []string, loadBranches func() *mitev1.JobBranchSet) (keep, orphans []string) {
	var (
		branches *mitev1.JobBranchSet
		loaded   bool
	)
	for _, jobPath := range deferred {
		c := jobCreatorFor(jenkinsHome, jobPath)
		switch {
		case !c.canCreate:
			orphans = append(orphans, jobPath)
		case c.folder == "":
			keep = append(keep, jobPath)
		default:
			if !loaded {
				loaded = true
				if loadBranches != nil {
					branches = loadBranches()
				}
			}
			if scanOmitsJob(branches, c.folder, jobPath) {
				orphans = append(orphans, jobPath)
			} else {
				keep = append(keep, jobPath)
			}
		}
	}
	return keep, orphans
}

// scanOmitsJob reports whether a completed, successful scan of folder,
// or of a repository below it, lists no child on the path to jobPath.
// A missing row, a scan that did not succeed, truncated heads, or a job
// path outside folder all answer false.
func scanOmitsJob(set *mitev1.JobBranchSet, folder, jobPath string) bool {
	if set == nil {
		return false
	}
	for {
		row := branchProjectRow(set, folder)
		if row == nil || row.IndexResult != branchIndexSucceeded || row.HeadsTruncated {
			return false
		}
		prefix := folder + "/"
		if !strings.HasPrefix(jobPath, prefix) {
			return false
		}
		rest := strings.TrimPrefix(jobPath, prefix)
		child := folder + "/" + rest
		if next := strings.Index(rest, "/"); next >= 0 {
			child = folder + "/" + rest[:next]
		}
		if !hasBranchHead(row, child) {
			return true
		}
		if child == jobPath {
			return false
		}
		folder = child
	}
}

// branchProjectRow returns the inventory row of the project at jobPath, or
// nil when the set holds no such row.
func branchProjectRow(set *mitev1.JobBranchSet, jobPath string) *mitev1.JobBranchProject {
	for _, item := range set.Items {
		if item.JobPath == jobPath {
			return item
		}
	}
	return nil
}

// hasBranchHead reports whether the row lists a head at jobPath.
func hasBranchHead(row *mitev1.JobBranchProject, jobPath string) bool {
	for _, head := range row.Heads {
		if head.JobPath == jobPath {
			return true
		}
	}
	return false
}

// jobCreatorFor reports whether an ancestor of jobPath can still create it
// later. The walk starts at the direct parent and goes up one level at a
// time. The first ancestor that is a directory without a config.xml, or that
// has a config.xml, decides. A config.xml that names a multibranch project or
// an organization folder keeps the job, and the result names that folder. An
// organization folder indexes the repositories below it, so a job under a
// repository that is not indexed yet keeps its records. A directory without a
// config.xml keeps the job too, because Jenkins may not have written the
// config yet. An absent directory passes the walk on to its own parent. A
// config.xml of any other kind cannot create the job. Any other read error
// keeps the job, so an unreadable ancestor never drops records by mistake. A
// top-level job has no parent and cannot be created later.
func jobCreatorFor(jenkinsHome, jobPath string) jobCreator {
	idx := strings.LastIndex(jobPath, "/")
	if idx < 0 {
		return jobCreator{}
	}
	for ancestor := jobPath[:idx]; ancestor != ""; {
		// Map the ancestor through parseRehydrationPath, the one job-path rule.
		parsed, err := parseRehydrationPath("jobs/" + strings.ReplaceAll(ancestor, "/", "/jobs/") + "/nextBuildNumber")
		if err != nil {
			return jobCreator{canCreate: true}
		}
		dir := filepath.Join(append([]string{jenkinsHome}, parsed.jobDirParts...)...)
		root, err := rootElementName(filepath.Join(dir, "config.xml"))
		if err == nil {
			if strings.HasSuffix(root, "MultiBranchProject") || strings.HasSuffix(root, "OrganizationFolder") {
				return jobCreator{canCreate: true, folder: ancestor}
			}
			return jobCreator{}
		}
		if !errors.Is(err, os.ErrNotExist) {
			return jobCreator{canCreate: true}
		}
		info, statErr := os.Stat(dir)
		if statErr == nil {
			return jobCreator{canCreate: info.IsDir()}
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return jobCreator{canCreate: true}
		}
		next := strings.LastIndex(ancestor, "/")
		if next < 0 {
			return jobCreator{}
		}
		ancestor = ancestor[:next]
	}
	return jobCreator{}
}

// rootElementName returns the name of the root element of the XML file at
// path. It reads at most 64 KiB, which holds the root start tag of a Jenkins
// config.xml.
func rootElementName(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	dec := xml.NewDecoder(jenkinsXMLReader(io.LimitReader(f, 64<<10)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

// jenkinsXMLVersion11 matches the version attribute of a declaration that
// names version 1.1. The XML grammar allows white space around the equals
// sign and either quote mark.
var jenkinsXMLVersion11 = regexp.MustCompile(`(version\s*=\s*)(['"])1\.1(['"])`)

// jenkinsXMLHeadBytes is how much of a stream jenkinsXMLReader inspects.
// Jenkins writes a declaration of 38 bytes. A longer declaration passes
// through unchanged.
const jenkinsXMLHeadBytes = 1024

// jenkinsXMLReader returns a reader over r whose leading XML declaration
// names version 1.0. Jenkins saves every item with version 1.1, which
// encoding/xml refuses, and the two versions differ in nothing the mite
// reads. A stream with no declaration passes through unchanged.
func jenkinsXMLReader(r io.Reader) io.Reader {
	head := make([]byte, jenkinsXMLHeadBytes)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return io.MultiReader(bytes.NewReader(head[:n]), errReader{err: err})
	}
	head = head[:n]
	if end := bytes.Index(head, []byte("?>")); bytes.HasPrefix(head, []byte("<?xml")) && end >= 0 {
		decl := string(head[:end+2])
		fixed := jenkinsXMLVersion11.ReplaceAllString(decl, "${1}${2}1.0${3}")
		head = append([]byte(fixed), head[end+2:]...)
	}
	return io.MultiReader(bytes.NewReader(head), r)
}

// errReader answers the same read error on every call.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// rehydrationApplyResult is the outcome of applying one RehydrationBundle.
type rehydrationApplyResult struct {
	// deferredJobs holds every job path the mite could not seed because
	// the job's config.xml does not exist yet.
	deferredJobs []string
}

// buildFiles groups the build.xml and changelog0.xml entries of one build
// so applyRehydrationBundle can write the changelog first regardless of the
// order the bundle lists them in.
type buildFiles struct {
	jobDir      string
	jobPath     string
	jobDirParts []string
	buildNumber int
	buildXML    *mitev1.RehydrationFile
	changelog   *mitev1.RehydrationFile
}

// applyRehydrationBundle writes every file of bundle under jenkinsHomeDir.
// A path that fails the allow-list check, or content whose inflated size
// does not match its declared raw_size, fails the whole section. Every
// other write rule — skip an existing build, defer a job with no
// config.xml, never lower nextBuildNumber — only skips that one file.
func applyRehydrationBundle(jenkinsHomeDir string, bundle *mitev1.RehydrationBundle) (rehydrationApplyResult, error) {
	builds := make(map[string]*buildFiles)
	var buildOrder []string
	type numberFile struct {
		jobDir  string
		jobPath string
		file    *mitev1.RehydrationFile
	}
	var numbers []numberFile

	for _, f := range bundle.Files {
		parsed, _, err := checkRehydrationPath(jenkinsHomeDir, f.Path)
		if err != nil {
			return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
		}
		jobDir := filepath.Join(append([]string{jenkinsHomeDir}, parsed.jobDirParts...)...)

		switch parsed.kind {
		case rehydrationPathNextBuildNumber:
			numbers = append(numbers, numberFile{jobDir: jobDir, jobPath: parsed.jobPath, file: f})
		default:
			key := jobDir + "\x00" + strconv.Itoa(parsed.buildNumber)
			bf, ok := builds[key]
			if !ok {
				bf = &buildFiles{
					jobDir:      jobDir,
					jobPath:     parsed.jobPath,
					jobDirParts: parsed.jobDirParts,
					buildNumber: parsed.buildNumber,
				}
				builds[key] = bf
				buildOrder = append(buildOrder, key)
			}
			if parsed.kind == rehydrationPathBuildXML {
				bf.buildXML = f
			} else {
				bf.changelog = f
			}
		}
	}

	deferredSeen := make(map[string]struct{})
	var deferredJobs []string
	markDeferred := func(jobPath string) {
		if _, ok := deferredSeen[jobPath]; ok {
			return
		}
		deferredSeen[jobPath] = struct{}{}
		deferredJobs = append(deferredJobs, jobPath)
	}

	resolver := newJobDirResolver(jenkinsHomeDir)

	// nextBuildNumber: defer a missing job, and never lower a stored value.
	for _, n := range numbers {
		dir := resolver.resolveJobDir(n.jobPath, n.jobDir)
		if !jobConfigExists(dir.absDir) {
			markDeferred(n.jobPath)
			continue
		}
		if err := applyNextBuildNumber(dir.absDir, n.file); err != nil {
			return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
		}
	}

	// Build records: defer a missing job, skip an existing build directory,
	// and write the changelog before the build.xml of the same build.
	for _, key := range buildOrder {
		bf := builds[key]
		dir := resolver.resolveJobDir(bf.jobPath, bf.jobDir)
		if !jobConfigExists(dir.absDir) {
			markDeferred(bf.jobPath)
			continue
		}
		buildNumber := strconv.Itoa(bf.buildNumber)
		buildDir := filepath.Join(dir.absDir, "builds", buildNumber)
		// A moved job's build path is not in checkRehydrationPath's check,
		// so the chain under the resolved job directory is checked here.
		if err := checkNoSymlinkInParents(jenkinsHomeDir, filepath.Join(buildDir, buildFileName)); err != nil {
			return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
		}
		if buildDirExists(buildDir) {
			continue
		}
		if bf.changelog != nil {
			if err := writeInflatedRehydrationFile(filepath.Join(buildDir, changelogFileName), bf.changelog, nil); err != nil {
				return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
			}
		}
		if bf.buildXML != nil {
			var rewrite func([]byte) []byte
			if dir.moved {
				// The operator names the changelog by the logical job path.
				// A moved job's changelog sits under the directory it resolved to.
				tail := buildNumber + "/" + changelogFileName
				logicalRel := strings.Join(bf.jobDirParts, "/") + "/builds/" + tail
				movedRel := strings.Join(dir.parts, "/") + "/builds/" + tail
				rewrite = func(data []byte) []byte { return rebaseChangelogFile(data, logicalRel, movedRel) }
			}
			if err := writeInflatedRehydrationFile(filepath.Join(buildDir, buildFileName), bf.buildXML, rewrite); err != nil {
				return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
			}
		}
	}

	return rehydrationApplyResult{deferredJobs: deferredJobs}, nil
}

// writeInflatedRehydrationFile inflates f's gzip content, applies rewrite
// when it is not nil, and writes the result to absPath.
func writeInflatedRehydrationFile(absPath string, f *mitev1.RehydrationFile, rewrite func([]byte) []byte) error {
	data, err := inflateRehydrationContent(f.GzipContent, f.RawSize)
	if err != nil {
		return fmt.Errorf("file %s: %w", f.Path, err)
	}
	if rewrite != nil {
		data = rewrite(data)
	}
	if err := atomicWriteRehydrationFile(absPath, data); err != nil {
		return fmt.Errorf("file %s: %w", f.Path, err)
	}
	return nil
}

// rebaseChangelogFile replaces the logical changelog path in a build.xml
// <changelogFile> element with the path of the directory the job resolved
// to. The operator writes the logical path, which names no directory for a
// branch job. Only the first match changes.
func rebaseChangelogFile(data []byte, logicalRel, movedRel string) []byte {
	oldTail := []byte(xmlEscape("/"+logicalRel) + "</changelogFile>")
	newTail := []byte(xmlEscape("/"+movedRel) + "</changelogFile>")
	return bytes.Replace(data, oldTail, newTail, 1)
}

// xmlEscape escapes s the way encoding/xml writes character data.
func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// decodeItemName reverses jenkins.branch.NameEncoder for one item name
// segment. A branch or repository name that holds a reserved character
// is stored with that character as %XX. Any other %XX stays literal.
func decodeItemName(seg string) string {
	switch strings.ToUpper(seg) {
	case "%00":
		return ""
	case "%2E":
		return "."
	case "%2E.":
		return ".."
	}
	if !strings.Contains(seg, "%") {
		return seg
	}
	return itemNameDecoder.Replace(seg)
}

// itemNameDecoder holds the seven escapes NameEncoder writes, in both hex
// cases.
var itemNameDecoder = strings.NewReplacer(
	"%23", "#", "%25", "%", "%2F", "/", "%3F", "?", "%5B", "[", "%5D", "]", "%5C", `\`,
	"%2f", "/", "%3f", "?", "%5b", "[", "%5d", "]", "%5c", `\`,
)

// maxItemConfigBytes bounds the read of one item config.xml. A branch or
// repository config holds its name in the first part of the file.
const maxItemConfigBytes = 1 << 20

// itemNameFromConfig returns the Jenkins item name that the config.xml at
// configPath declares. sub is "branches" for a branch job of a multibranch
// project, whose name is its head name, and "jobs" for a repository of an
// organization folder, whose name is its ProjectNameProperty. ok is false
// when the file holds no such name.
func itemNameFromConfig(configPath, sub string) (name string, ok bool, err error) {
	f, err := os.Open(configPath)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = f.Close() }()

	match := func(stack []string) bool {
		if sub == "branches" {
			return len(stack) == 6 && stack[1] == "properties" &&
				strings.HasSuffix(stack[2], "BranchJobProperty") &&
				stack[3] == "branch" && stack[4] == "head" && stack[5] == "name"
		}
		return len(stack) == 4 && stack[1] == "properties" &&
			stack[2] == "jenkins.branch.ProjectNameProperty" && stack[3] == "name"
	}

	dec := xml.NewDecoder(jenkinsXMLReader(io.LimitReader(f, maxItemConfigBytes)))
	var stack []string
	captureDepth := 0
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if captureDepth == 0 && match(stack) {
				captureDepth = len(stack)
				text.Reset()
			}
		case xml.CharData:
			if captureDepth > 0 {
				text.Write(t)
			}
		case xml.EndElement:
			if captureDepth > 0 && len(stack) == captureDepth {
				return text.String(), true, nil
			}
			stack = stack[:len(stack)-1]
		}
	}
}

// resolvedJobDir is the directory one job path resolves to. moved is set
// when the job is not at its logical directory and sits at parts instead.
type resolvedJobDir struct {
	absDir string
	// parts are the path parts under JENKINS_HOME, set only when moved.
	parts []string
	moved bool
}

// jobDirResolver finds the directory of a job that is not at its logical
// path. A branch job of a multibranch project sits under branches/<dir>,
// and a repository of an organization folder can sit under jobs/<dir>
// with a mangled directory name. The resolver reads each parent's own
// config.xml and indexes its children by their Jenkins item name, so the
// mite never guesses a directory name. A resolver lives for one apply.
type jobDirResolver struct {
	home    string
	indexes map[string]map[string]string
	dirs    map[string]resolvedJobDir
}

// newJobDirResolver returns a resolver for the Jenkins home at home.
func newJobDirResolver(home string) *jobDirResolver {
	return &jobDirResolver{
		home:    home,
		indexes: make(map[string]map[string]string),
		dirs:    make(map[string]resolvedJobDir),
	}
}

// childIndex maps the Jenkins item name of each child directory of
// parentDir/sub to the directory name. Symbolic links and entries with
// an invalid name are skipped. The index is built once per apply.
func (r *jobDirResolver) childIndex(parentDir, sub string) map[string]string {
	key := parentDir + "\x00" + sub
	if idx, ok := r.indexes[key]; ok {
		return idx
	}
	idx := make(map[string]string)
	r.indexes[key] = idx
	entries, err := os.ReadDir(filepath.Join(parentDir, sub))
	if err != nil {
		return idx
	}
	for _, e := range entries {
		name := e.Name()
		if validateJobSegment(name) != nil {
			continue
		}
		dir := filepath.Join(parentDir, sub, name)
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		itemName, ok, err := itemNameFromConfig(filepath.Join(dir, "config.xml"), sub)
		if err != nil || !ok {
			continue
		}
		if _, dup := idx[itemName]; !dup {
			idx[itemName] = name
		}
	}
	return idx
}

// resolve walks jobPath from the Jenkins home. A segment under a
// multibranch project maps through the branch index, and a segment under
// an organization folder maps through the repository index. Any other
// segment keeps the logical "jobs/<seg>" form. ok is false when a segment
// has no indexed directory or the job has no config.xml at the end.
func (r *jobDirResolver) resolve(jobPath string) (parts []string, ok bool) {
	dir := r.home
	for _, seg := range strings.Split(jobPath, "/") {
		kind, err := rootElementName(filepath.Join(dir, "config.xml"))
		if err != nil {
			// The home directory may hold no config.xml yet. Its children
			// then keep the logical form, as a plain folder does.
			if dir != r.home || !errors.Is(err, os.ErrNotExist) {
				return nil, false
			}
			kind = ""
		}
		var sub string
		switch {
		case strings.HasSuffix(kind, "MultiBranchProject"):
			sub = "branches"
		case strings.HasSuffix(kind, "OrganizationFolder"):
			sub = "jobs"
		default:
			parts = append(parts, "jobs", seg)
			dir = filepath.Join(dir, "jobs", seg)
			continue
		}
		d, found := r.childIndex(dir, sub)[decodeItemName(seg)]
		if !found {
			return nil, false
		}
		parts = append(parts, sub, d)
		dir = filepath.Join(dir, sub, d)
	}
	return parts, jobConfigExists(dir)
}

// resolveJobDir returns the directory of the job at jobPath. A job with a
// config.xml at logicalDir keeps that directory. A job without one resolves
// through its parents. When no directory resolves, logicalDir returns, so
// the caller defers the job as before.
func (r *jobDirResolver) resolveJobDir(jobPath, logicalDir string) resolvedJobDir {
	if cached, ok := r.dirs[logicalDir]; ok {
		return cached
	}
	res := resolvedJobDir{absDir: logicalDir}
	if !jobConfigExists(logicalDir) {
		if parts, ok := r.resolve(jobPath); ok {
			rel := strings.Join(parts, "/")
			abs, err := confineRehydrationPath(r.home, rel)
			if err == nil && checkNoSymlinkInParents(r.home, filepath.Join(abs, "config.xml")) == nil {
				res = resolvedJobDir{absDir: abs, parts: parts, moved: true}
			}
		}
	}
	r.dirs[logicalDir] = res
	return res
}

// applyNextBuildNumber inflates f's gzip content, parses it as the proposed
// nextBuildNumber value, and writes it only when jobDir holds no
// nextBuildNumber file yet or the stored value is lower. It never lowers a
// stored value.
func applyNextBuildNumber(jobDir string, f *mitev1.RehydrationFile) error {
	data, err := inflateRehydrationContent(f.GzipContent, f.RawSize)
	if err != nil {
		return fmt.Errorf("file %s: %w", f.Path, err)
	}
	proposed, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("file %s: invalid nextBuildNumber content: %w", f.Path, err)
	}

	nbPath := filepath.Join(jobDir, "nextBuildNumber")
	existingData, readErr := os.ReadFile(nbPath)
	switch {
	case readErr == nil:
		if existing, perr := strconv.Atoi(strings.TrimSpace(string(existingData))); perr == nil && existing >= proposed {
			return nil
		}
	case !os.IsNotExist(readErr):
		return fmt.Errorf("read existing nextBuildNumber %s: %w", nbPath, readErr)
	}

	return atomicWriteRehydrationFile(nbPath, data)
}
