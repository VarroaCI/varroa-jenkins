// Package forgeurl holds the input rules of the forge path: repository URLs,
// branch names, revisions, file paths, file sets, titles, and bodies. It
// imports the standard library only, so the API tier can apply the rules
// without importing the forge client.
package forgeurl

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// RuleError names the argument that a rule refuses.
type RuleError struct {
	Arg    string
	Reason string
}

// Error returns the argument name and the reason.
func (e *RuleError) Error() string { return e.Arg + ": " + e.Reason }

// RepoURL is a normalized repository location on a forge host.
type RepoURL struct {
	Host  string
	Owner string
	Name  string
}

// String returns the canonical form https://<host>/<owner>/<name>.
func (u RepoURL) String() string {
	host := u.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "https://" + host + "/" + u.Owner + "/" + u.Name
}

var segmentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func refuse(arg, reason string) error {
	return &RuleError{Arg: arg, Reason: reason}
}

// NormalizeRepoURL accepts only https://host/owner/name with an optional
// .git suffix, a trailing slash, or port 443. It refuses every other form.
func NormalizeRepoURL(raw string) (RepoURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return RepoURL{}, refuse("repo", "not a valid URL")
	}
	if u.Scheme != "https" {
		return RepoURL{}, refuse("repo", "scheme must be https")
	}
	if u.Opaque != "" || u.User != nil {
		return RepoURL{}, refuse("repo", "userinfo and opaque URLs are refused")
	}
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "?") {
		return RepoURL{}, refuse("repo", "a query is refused")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return RepoURL{}, refuse("repo", "a fragment is refused")
	}
	if u.RawPath != "" || strings.Contains(u.Path, "%") {
		return RepoURL{}, refuse("repo", "encoded path characters are refused")
	}
	host := u.Hostname()
	if host == "" {
		return RepoURL{}, refuse("repo", "host is required")
	}
	if p := u.Port(); p != "" && p != "443" {
		return RepoURL{}, refuse("repo", "only port 443 is allowed")
	}
	host = strings.ToLower(host)
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsGlobalUnicast() {
			return RepoURL{}, refuse("repo", "local addresses are refused")
		}
	}
	path := strings.TrimSuffix(u.Path, "/")
	if !strings.HasPrefix(path, "/") {
		return RepoURL{}, refuse("repo", "path must be /<owner>/<name>")
	}
	segments := strings.Split(path[1:], "/")
	if len(segments) != 2 {
		return RepoURL{}, refuse("repo", "path must be /<owner>/<name>")
	}
	for _, s := range segments {
		if !segmentPattern.MatchString(s) || s == "." || s == ".." {
			return RepoURL{}, refuse("repo", "invalid path segment")
		}
	}
	name := strings.TrimSuffix(segments[1], ".git")
	if name == "" {
		return RepoURL{}, refuse("repo", "repository name is empty")
	}
	return RepoURL{Host: host, Owner: segments[0], Name: name}, nil
}

// MaxBranchLen is the longest accepted branch name.
const MaxBranchLen = 200

// ValidateBranch applies the branch-name rules of the design, section 10.2.
func ValidateBranch(name string) error {
	if len(name) < 1 || len(name) > MaxBranchLen {
		return refuse("branch", "length must be 1 to 200 bytes")
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") {
		return refuse("branch", "'..' and '//' are refused")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return refuse("branch", "a leading '-' or '/' is refused")
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") {
		return refuse("branch", "a trailing '/' or '.lock' is refused")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f || c == ' ' || strings.IndexByte("~^:?*[\\", c) >= 0 {
			return refuse("branch", "a forbidden character is present")
		}
	}
	return nil
}

// IsCommitSHA reports whether s is a 40 or 64 digit hexadecimal object name.
func IsCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
		if !ok {
			return false
		}
	}
	return true
}

// ValidateRevision accepts a commit SHA or a branch name.
func ValidateRevision(rev string) error {
	if IsCommitSHA(rev) || ValidateBranch(rev) == nil {
		return nil
	}
	return refuse("revision", "must be a commit SHA or a valid branch name")
}

// CleanReadPath returns the repository path without a trailing slash. An
// empty result names the repository root.
func CleanReadPath(p string) (string, error) {
	p = strings.TrimSuffix(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", refuse("path", "a leading '/' is refused")
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return "", refuse("path", "control characters are refused")
		}
	}
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." || s == ".." {
			return "", refuse("path", "empty, '.', and '..' segments are refused")
		}
	}
	return p, nil
}

// ValidateFilePath accepts a non-root path that holds no trailing slash.
func ValidateFilePath(p string) error {
	return cleanFilePath(p)
}

// cleanFilePath applies the file-path rule.
func cleanFilePath(p string) error {
	if strings.HasSuffix(p, "/") {
		return refuse("path", "a file path has no trailing slash")
	}
	c, err := CleanReadPath(p)
	if err != nil {
		return err
	}
	if c == "" {
		return refuse("path", "a file path names no root")
	}
	return nil
}

// File is one file of a proposed commit.
type File struct {
	Path    string
	Content string
}

// Limits for proposed files, titles, and bodies.
const (
	MaxFiles      = 10
	MaxFileBytes  = 64 << 10
	MaxFilesTotal = 256 << 10
	MaxTitleLen   = 200
	MaxBodyBytes  = 8 << 10
)

// ValidateFiles applies the count, path, content, and size rules to a file set.
func ValidateFiles(files []File) error {
	if len(files) < 1 || len(files) > MaxFiles {
		return refuse("files", fmt.Sprintf("count must be 1 to %d", MaxFiles))
	}
	seen := make(map[string]bool, len(files))
	total := 0
	for i, f := range files {
		argPath := fmt.Sprintf("files[%d].path", i)
		argContent := fmt.Sprintf("files[%d].content", i)
		if err := cleanFilePath(f.Path); err != nil {
			return refuse(argPath, err.(*RuleError).Reason)
		}
		if seen[f.Path] {
			return refuse(argPath, "duplicate path")
		}
		seen[f.Path] = true
		if !utf8.ValidString(f.Content) {
			return refuse(argContent, "content must be valid UTF-8")
		}
		if strings.IndexByte(f.Content, 0) >= 0 {
			return refuse(argContent, "content must not hold NUL")
		}
		if len(f.Content) > MaxFileBytes {
			return refuse(argContent, "content exceeds 64 KiB")
		}
		total += len(f.Content)
	}
	if total > MaxFilesTotal {
		return refuse("files", "total content exceeds 256 KiB")
	}
	return nil
}

// ValidateTitle accepts 1 to 200 runes with no line break.
func ValidateTitle(t string) error {
	n := utf8.RuneCountInString(t)
	if n < 1 || n > MaxTitleLen {
		return refuse("title", "length must be 1 to 200 characters")
	}
	if strings.ContainsAny(t, "\n\r") {
		return refuse("title", "line breaks are refused")
	}
	return nil
}

// ValidateBody accepts a body of at most 8 KiB.
func ValidateBody(b string) error {
	if len(b) > MaxBodyBytes {
		return refuse("body", "body exceeds 8 KiB")
	}
	return nil
}

// MaxRelayBodyBytes bounds a pull-request body after the BFF appends its fixed line.
const MaxRelayBodyBytes = MaxBodyBytes + 1024

// MaxCommitMessageBytes bounds a commit message: the title and one trailer.
const MaxCommitMessageBytes = 1024

// ValidateRelayBody accepts a relayed body of at most MaxRelayBodyBytes.
func ValidateRelayBody(b string) error {
	if len(b) > MaxRelayBodyBytes {
		return refuse("body", "body exceeds 9 KiB")
	}
	return nil
}

// ValidateCommitMessage accepts a non-empty UTF-8 message without NUL of at
// most MaxCommitMessageBytes.
func ValidateCommitMessage(m string) error {
	switch {
	case m == "":
		return refuse("commitMessage", "message is required")
	case !utf8.ValidString(m):
		return refuse("commitMessage", "message must be valid UTF-8")
	case strings.IndexByte(m, 0) >= 0:
		return refuse("commitMessage", "message must not hold NUL")
	case len(m) > MaxCommitMessageBytes:
		return refuse("commitMessage", "message exceeds 1 KiB")
	}
	return nil
}
