package rehydrate

import (
	"path"
	"strconv"
	"strings"
)

// buildFileName and changelogFileName are the two file names the
// mite's "builds" path shape accepts, per design.md D5 and
// cmd/mite/rehydrate.go.
const (
	buildFileName       = "build.xml"
	changelogFileName   = "changelog0.xml"
	nextBuildNumberName = "nextBuildNumber"
)

// jobDirParts inserts "jobs" before every segment of jobPath:
// "payments/api/main" becomes ["jobs","payments","jobs","api","jobs","main"].
func jobDirParts(jobPath string) []string {
	segs := strings.Split(jobPath, "/")
	parts := make([]string, 0, len(segs)*2)
	for _, s := range segs {
		parts = append(parts, "jobs", s)
	}
	return parts
}

// BuildXMLPath answers the allow-listed relative path (under
// JENKINS_HOME, "/"-joined, no leading slash) of one build's
// build.xml.
func BuildXMLPath(jobPath string, buildNumber int64) string {
	return buildPath(jobPath, buildNumber, buildFileName)
}

// ChangelogFilePath answers the allow-listed relative path of one
// build's changelog0.xml.
func ChangelogFilePath(jobPath string, buildNumber int64) string {
	return buildPath(jobPath, buildNumber, changelogFileName)
}

func buildPath(jobPath string, buildNumber int64, file string) string {
	parts := jobDirParts(jobPath)
	parts = append(parts, "builds", strconv.FormatInt(buildNumber, 10), file)
	return path.Join(parts...)
}

// NextBuildNumberPath answers the allow-listed relative path of a
// job's nextBuildNumber file.
func NextBuildNumberPath(jobPath string) string {
	parts := jobDirParts(jobPath)
	parts = append(parts, nextBuildNumberName)
	return path.Join(parts...)
}

// ChangelogAbsolutePath answers the absolute path RenderBuildXML
// writes into <checkouts>'s changelogFile element: jenkinsHome joined
// with ChangelogFilePath's relative path. jenkinsHome comes from the
// JENKINS_HOME value of the cell's Jenkins container; a value that
// does not match the cell's real JENKINS_HOME makes Jenkins find no
// file, which loses that run's change set and nothing else (design.md
// D1a).
func ChangelogAbsolutePath(jenkinsHome, jobPath string, buildNumber int64) string {
	return path.Join(jenkinsHome, ChangelogFilePath(jobPath, buildNumber))
}
