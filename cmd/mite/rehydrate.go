// rehydrate.go applies a RehydrationBundle under JENKINS_HOME. It writes
// only allow-listed paths, defers a job that does not exist yet, never
// overwrites a real build, and never lowers a stored nextBuildNumber. See
// design D5 and the build-record-rehydration spec.
package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// rehydrationPathKind identifies which of the three accepted file shapes a
// rehydration path matches.
type rehydrationPathKind int

const (
	rehydrationPathBuildXML rehydrationPathKind = iota
	rehydrationPathChangelog
	rehydrationPathNextBuildNumber
)

// changelogFileName and buildFileName are the two file names the "builds"
// shape accepts.
const (
	changelogFileName = "changelog0.xml"
	buildFileName     = "build.xml"
)

// parsedRehydrationPath is one validated rehydration file path.
type parsedRehydrationPath struct {
	kind rehydrationPathKind
	// jobPath is the job's human path, "/"-joined ("team/pipeline").
	jobPath string
	// jobDirParts are the filesystem path parts under jenkinsHomeDir that
	// name the job's own directory ("jobs", "team", "jobs", "pipeline").
	jobDirParts []string
	// buildNumber is set only for rehydrationPathBuildXML and
	// rehydrationPathChangelog.
	buildNumber int
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

// parseRehydrationPath checks relPath against the three accepted shapes:
//
//	jobs/<seg>(/jobs/<seg>)*/builds/<n>/build.xml
//	jobs/<seg>(/jobs/<seg>)*/builds/<n>/changelog0.xml
//	jobs/<seg>(/jobs/<seg>)*/nextBuildNumber
//
// Any other shape fails. relPath uses forward slashes, matching the wire
// format the operator sends.
func parseRehydrationPath(relPath string) (parsedRehydrationPath, error) {
	if relPath == "" {
		return parsedRehydrationPath{}, errors.New("rehydration path is empty")
	}
	parts := strings.Split(relPath, "/")

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
	case len(rest) == 3 && rest[0] == "builds" && (rest[2] == buildFileName || rest[2] == changelogFileName):
		n, err := parseBuildNumber(rest[1])
		if err != nil {
			return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: %w", relPath, err)
		}
		parsed.buildNumber = n
		if rest[2] == buildFileName {
			parsed.kind = rehydrationPathBuildXML
		} else {
			parsed.kind = rehydrationPathChangelog
		}
		return parsed, nil
	default:
		return parsedRehydrationPath{}, fmt.Errorf("rehydration path %q: does not match an accepted shape", relPath)
	}
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
	jobDir    string
	jobPath   string
	buildDir  string
	buildXML  *mitev1.RehydrationFile
	changelog *mitev1.RehydrationFile
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
		parsed, absPath, err := checkRehydrationPath(jenkinsHomeDir, f.Path)
		if err != nil {
			return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
		}
		jobDir := filepath.Join(append([]string{jenkinsHomeDir}, parsed.jobDirParts...)...)

		switch parsed.kind {
		case rehydrationPathNextBuildNumber:
			numbers = append(numbers, numberFile{jobDir: jobDir, jobPath: parsed.jobPath, file: f})
		default:
			buildDir := filepath.Dir(absPath)
			key := jobDir + "\x00" + strconv.Itoa(parsed.buildNumber)
			bf, ok := builds[key]
			if !ok {
				bf = &buildFiles{jobDir: jobDir, jobPath: parsed.jobPath, buildDir: buildDir}
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

	// nextBuildNumber: defer a missing job, and never lower a stored value.
	for _, n := range numbers {
		if !jobConfigExists(n.jobDir) {
			markDeferred(n.jobPath)
			continue
		}
		if err := applyNextBuildNumber(n.jobDir, n.file); err != nil {
			return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
		}
	}

	// Build records: defer a missing job, skip an existing build directory,
	// and write the changelog before the build.xml of the same build.
	for _, key := range buildOrder {
		bf := builds[key]
		if !jobConfigExists(bf.jobDir) {
			markDeferred(bf.jobPath)
			continue
		}
		if buildDirExists(bf.buildDir) {
			continue
		}
		if bf.changelog != nil {
			if err := writeInflatedRehydrationFile(filepath.Join(bf.buildDir, changelogFileName), bf.changelog); err != nil {
				return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
			}
		}
		if bf.buildXML != nil {
			if err := writeInflatedRehydrationFile(filepath.Join(bf.buildDir, buildFileName), bf.buildXML); err != nil {
				return rehydrationApplyResult{}, fmt.Errorf("rehydration: %w", err)
			}
		}
	}

	return rehydrationApplyResult{deferredJobs: deferredJobs}, nil
}

// writeInflatedRehydrationFile inflates f's gzip content and writes it to
// absPath.
func writeInflatedRehydrationFile(absPath string, f *mitev1.RehydrationFile) error {
	data, err := inflateRehydrationContent(f.GzipContent, f.RawSize)
	if err != nil {
		return fmt.Errorf("file %s: %w", f.Path, err)
	}
	if err := atomicWriteRehydrationFile(absPath, data); err != nil {
		return fmt.Errorf("file %s: %w", f.Path, err)
	}
	return nil
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
