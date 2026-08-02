package bundle

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// AllowedHostsAnnotation is the annotation key on a Secret that restricts
// basic-auth git credentials to specific git hosts. Only basic-auth secrets
// are affected; SSH secrets (ssh-privatekey) are exempt because the private
// key material is not transmitted to the remote host during a normal handshake.
const AllowedHostsAnnotation = "varroa.dev/allowed-hosts"

// GitAuth holds authentication credentials for git operations.
// Exactly one mechanism must be set.
type GitAuth struct {
	// Username and Password/Token for HTTPS basic auth.
	// +optional
	Username string
	// +optional
	Password string

	// SSHPrivateKey is the raw private key for SSH auth.
	// When set, the key is written to a temp file (mode 0600) and used via
	// GIT_SSH_COMMAND. The temp file is removed after the git operation.
	// +optional
	SSHPrivateKey string

	// KnownHosts, when set, pins the SSH host keys. It is written to a temp
	// file (mode 0600) and used via GIT_SSH_COMMAND with
	// StrictHostKeyChecking=yes. When empty, host keys are trusted on first
	// use (StrictHostKeyChecking=accept-new).
	// +optional
	KnownHosts string
}

// IsEmpty returns true if no auth mechanism is set.
func (a *GitAuth) IsEmpty() bool {
	if a == nil {
		return true
	}
	return a.Username == "" && a.Password == "" && a.SSHPrivateKey == ""
}

// Mechanism returns the auth mechanism: "basic", "ssh", or "" if unset.
func (a *GitAuth) Mechanism() string {
	if a == nil {
		return ""
	}
	if a.SSHPrivateKey != "" {
		return "ssh"
	}
	if a.Username != "" || a.Password != "" {
		return "basic"
	}
	return ""
}

// gitAuthEnv prepares environment variables for git auth. It returns the
// environment overrides and a cleanup function that must be called after the
// git command completes. For SSH auth, it writes a temp key file (mode 0600)
// and the cleanup removes it. For basic auth, it writes a temporary askpass
// script that feeds the password without leaking it into argv or ps output.
func gitAuthEnv(auth *GitAuth) ([]string, func(), error) {
	if auth == nil || auth.IsEmpty() {
		return nil, func() {}, nil
	}

	var env []string
	cleanup := func() {}
	tmpDir := os.TempDir()

	switch auth.Mechanism() {
	case "basic":
		// Write a GIT_ASKPASS script that responds with username or password
		// depending on the prompt. Uses os.CreateTemp to avoid PID-only
		// collisions under concurrency.
		askpassFile, err := os.CreateTemp(tmpDir, "varroa-git-askpass-*")
		if err != nil {
			return nil, func() {}, fmt.Errorf("create askpass script: %w", err)
		}
		username := auth.Username
		password := auth.Password
		script := "#!/bin/sh\ncase \"$1\" in\n*Username*|*username*|*login*) echo '" + escapeSingleQuotes(username) + "' ;;\n*) echo '" + escapeSingleQuotes(password) + "' ;;\nesac\n"
		if _, err := askpassFile.WriteString(script); err != nil {
			_ = askpassFile.Close()
			_ = os.Remove(askpassFile.Name())
			return nil, func() {}, fmt.Errorf("write askpass script: %w", err)
		}
		if err := askpassFile.Chmod(0700); err != nil {
			_ = askpassFile.Close()
			_ = os.Remove(askpassFile.Name())
			return nil, func() {}, fmt.Errorf("chmod askpass script: %w", err)
		}
		_ = askpassFile.Close()
		askpassPath := askpassFile.Name()
		cleanup = func() { _ = os.Remove(askpassPath) }

		env = append(env,
			"GIT_TERMINAL_PROMPT=0",
			fmt.Sprintf("GIT_ASKPASS=%s", askpassPath),
		)

	case "ssh":
		// Write SSH key to a temp file with restrictive permissions.
		tmpFile, err := os.CreateTemp(tmpDir, "varroa-git-ssh-*")
		if err != nil {
			return nil, func() {}, fmt.Errorf("create temp ssh key: %w", err)
		}
		if err := tmpFile.Chmod(0600); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
			return nil, func() {}, fmt.Errorf("chmod ssh key: %w", err)
		}
		keyContent := auth.SSHPrivateKey
		if !strings.HasSuffix(keyContent, "\n") {
			keyContent += "\n"
		}
		if _, err := tmpFile.WriteString(keyContent); err != nil {
			_ = tmpFile.Close()
			_ = os.Remove(tmpFile.Name())
			return nil, func() {}, fmt.Errorf("write ssh key: %w", err)
		}
		_ = tmpFile.Close()

		keyPath := tmpFile.Name()
		cleanup = func() { _ = os.Remove(keyPath) }

		knownHostsOpt := "-o StrictHostKeyChecking=accept-new"
		if auth.KnownHosts != "" {
			khFile, err := os.CreateTemp(tmpDir, "varroa-git-knownhosts-*")
			if err != nil {
				return nil, cleanup, fmt.Errorf("create known_hosts file: %w", err)
			}
			if err := khFile.Chmod(0600); err != nil {
				_ = khFile.Close()
				_ = os.Remove(khFile.Name())
				return nil, cleanup, fmt.Errorf("chmod known_hosts file: %w", err)
			}
			khContent := auth.KnownHosts
			if !strings.HasSuffix(khContent, "\n") { // mirrors the key-write logic above
				khContent += "\n"
			}
			if _, err := khFile.WriteString(khContent); err != nil {
				_ = khFile.Close()
				_ = os.Remove(khFile.Name())
				return nil, cleanup, fmt.Errorf("write known_hosts file: %w", err)
			}
			_ = khFile.Close()
			khPath := khFile.Name()
			prev := cleanup // capture the key-file cleanup and chain the kh-file removal onto it
			cleanup = func() { prev(); _ = os.Remove(khPath) }
			knownHostsOpt = fmt.Sprintf("-o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s", khPath)
		}
		sshCmd := fmt.Sprintf("ssh -i %s %s -o PasswordAuthentication=no", keyPath, knownHostsOpt)
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=%s", sshCmd))
	}

	return env, cleanup, nil
}

// escapeSingleQuotes escapes single quotes for use in a shell script.
func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

// GitCloner handles shallow clones of bundle repositories.
type GitCloner struct {
	// allowLocalTransport additionally permits file:// repo URLs. It is set ONLY via
	// AllowLocalTransportForTest so tests can clone local bare-repo fixtures; production
	// code never sets it and file:// stays blocked (both here and via GIT_ALLOW_PROTOCOL).
	allowLocalTransport bool
}

// AllowLocalTransportForTest permits file:// URLs on this cloner. It exists solely for
// tests that clone local bare-repo fixtures and must never be called by production code.
func (g *GitCloner) AllowLocalTransportForTest() { g.allowLocalTransport = true }

// NewGitCloner creates a new GitCloner.
func NewGitCloner() *GitCloner {
	return &GitCloner{}
}

// validateRepoURL rejects repository URLs whose transport is unsafe before the URL is
// handed to git. It is the primary defense against transport-helper RCE (ext::, fd::,
// file://). Allowed: https://, ssh://, and scp-like [user@]host:path. Everything else
// — http://, git://, file://, any "X::addr" transport helper, and unknown schemes — is
// rejected. redactURL is used in the error so credentials in userinfo don't leak.
func validateRepoURL(repoURL string, allowLocal bool) error {
	u := strings.TrimSpace(repoURL)
	if u == "" {
		return fmt.Errorf("repoURL is empty")
	}
	// 1. Scheme form: "<scheme>://...". Parse the scheme FIRST so an IPv6 literal
	//    like https://[::1]/repo (which contains "::") is judged by its scheme, not
	//    mistaken for a transport helper.
	if i := strings.Index(u, "://"); i >= 0 {
		scheme := strings.ToLower(u[:i])
		if allowLocal && scheme == "file" {
			return nil // test-only local bare-repo fixtures
		}
		switch scheme {
		case "https", "ssh":
			return nil
		default:
			return fmt.Errorf("repoURL %q uses a disallowed scheme (allowed: https, ssh)", redactURL(u))
		}
	}
	// 2. No "://": reject any git transport-helper syntax "<transport>::<address>"
	//    (ext::, fd::, and the generic form). This is the RCE vector.
	if strings.Contains(u, "::") {
		return fmt.Errorf("repoURL %q uses a git transport helper (\"::\"), which is not allowed", redactURL(u))
	}
	// 3. scp-like shorthand [user@]host:path — a ':' whose left side has no '/'.
	if i := strings.Index(u, ":"); i > 0 && !strings.Contains(u[:i], "/") {
		return nil
	}
	return fmt.Errorf("repoURL %q has no recognized transport (expected https://, ssh://, or scp-like git@host:path)", redactURL(u))
}

// Clone performs a shallow clone of a repository at the given revision.
// If revision looks like a full SHA (40 hex chars), the default branch is
// cloned and then the SHA is checked out — --branch only works with ref names.
// auth may be nil for public repositories.
func (g *GitCloner) Clone(repoURL, revision, targetDir string, auth *GitAuth) error {
	if err := validateRepoURL(repoURL, g.allowLocalTransport); err != nil {
		return err
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create target dir: %w", err)
	}

	isSHA := isCommitSHA(revision)

	authEnv, cleanup, err := gitAuthEnv(auth)
	if err != nil {
		return fmt.Errorf("setup git auth: %w", err)
	}
	defer cleanup()

	// Always disable interactive prompting — public repos should clone
	// anonymously, not prompt for credentials.
	gitEnv := append(os.Environ(), authEnv...)
	gitEnv = append(gitEnv, "GIT_TERMINAL_PROMPT=0")
	allowProto := "https:ssh"
	if g.allowLocalTransport {
		allowProto = "https:ssh:file"
	}
	gitEnv = append(gitEnv, "GIT_ALLOW_PROTOCOL="+allowProto)

	args := []string{"clone", "--depth=1", "--no-tags"}
	if revision != "" && !isSHA {
		args = append(args, "--branch", revision)
	}
	args = append(args, repoURL, targetDir)

	cmd := exec.Command("git", args...)
	cmd.Env = gitEnv
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %s", redactURL(repoURL), stderr.String())
	}

	if isSHA {
		cmd := exec.Command("git", "-C", targetDir, "fetch", "--depth=1", "origin", revision)
		cmd.Env = gitEnv
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("fetch revision %s: %s", revision, stderr.String())
		}
		cmd = exec.Command("git", "-C", targetDir, "checkout", "--quiet", revision)
		cmd.Env = gitEnv
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("checkout revision %s: %s", revision, stderr.String())
		}
	}

	return nil
}

// GitAuthFromSecret reads a Kubernetes Secret (as raw byte map) and returns a
// GitAuth. It supports kubernetes.io/basic-auth (username + password/token) and
// kubernetes.io/ssh-auth (ssh-privatekey). Returns a clear error for missing or
// malformed secrets. The returned error MUST NOT contain secret material.
func GitAuthFromSecret(data map[string][]byte, secretType string) (*GitAuth, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("secret is empty")
	}

	switch secretType {
	case "kubernetes.io/basic-auth":
		username := string(data["username"])
		password := string(data["password"])
		if password == "" {
			// Also check "token" key as an alternative.
			password = string(data["token"])
		}
		if username == "" && password == "" {
			return nil, fmt.Errorf("basic-auth secret missing username and password/token keys")
		}
		return &GitAuth{Username: username, Password: password}, nil

	case "kubernetes.io/ssh-auth":
		key := string(data["ssh-privatekey"])
		if key == "" {
			// Also check "privatekey" as an alternative key.
			key = string(data["privatekey"])
		}
		if key == "" {
			return nil, fmt.Errorf("ssh-auth secret missing ssh-privatekey key")
		}
		kh := string(data["known_hosts"])
		if kh == "" {
			kh = string(data["known-hosts"])
		}
		return &GitAuth{SSHPrivateKey: key, KnownHosts: kh}, nil

	default:
		// Fallback: check for well-known keys regardless of type.
		if username := string(data["username"]); username != "" {
			password := string(data["password"])
			if password == "" {
				password = string(data["token"])
			}
			return &GitAuth{Username: username, Password: password}, nil
		}
		if key := string(data["ssh-privatekey"]); key != "" {
			kh := string(data["known_hosts"])
			if kh == "" {
				kh = string(data["known-hosts"])
			}
			return &GitAuth{SSHPrivateKey: key, KnownHosts: kh}, nil
		}
		return nil, fmt.Errorf("unsupported secret type %q; expected kubernetes.io/basic-auth or kubernetes.io/ssh-auth", secretType)
	}
}

// redactURL strips user info from a URL for safe logging.
// It handles https://user:pass@host/path but leaves SCP-style URLs
// (git@github.com:org/repo) unchanged since git@ is a username, not a secret.
func redactURL(rawURL string) string {
	if idx := strings.Index(rawURL, "@"); idx >= 0 {
		protoEnd := strings.Index(rawURL, "://")
		if protoEnd >= 0 && protoEnd < idx {
			// URL-style: redact user:password portion.
			return rawURL[:protoEnd+3] + "***" + rawURL[idx:]
		}
		// SCP-style (git@host:path): check if there's a colon after @ with no ://
		// before it. This is a benign git username, not a secret.
		if !strings.Contains(rawURL[:idx], "://") {
			return rawURL // SCP-style, don't redact
		}
		return "***" + rawURL[idx:]
	}
	return rawURL
}

// Checkout switches to a specific revision in an existing repository.
func (g *GitCloner) Checkout(repoDir, revision string) error {
	cmd := exec.Command("git", "-C", repoDir, "checkout", "--quiet", revision)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout %s: %s", revision, stderr.String())
	}
	return nil
}

// RemoteSHA fetches the HEAD commit SHA for a remote branch or tag using
// git ls-remote. If revision is a 40-char hex SHA, it is returned directly
// to avoid an unnecessary ls-remote call that would return empty.
// auth may be nil for public repositories.
func (g *GitCloner) RemoteSHA(repoURL, revision string, auth *GitAuth) (string, error) {
	if err := validateRepoURL(repoURL, g.allowLocalTransport); err != nil {
		return "", err
	}
	if revision == "" {
		return "", fmt.Errorf("revision is required for git ls-remote")
	}
	if isCommitSHA(revision) {
		return revision, nil
	}

	authEnv, cleanup, err := gitAuthEnv(auth)
	if err != nil {
		return "", fmt.Errorf("setup git auth: %w", err)
	}
	defer cleanup()

	gitEnv := append(os.Environ(), authEnv...)
	gitEnv = append(gitEnv, "GIT_TERMINAL_PROMPT=0")
	allowProto := "https:ssh"
	if g.allowLocalTransport {
		allowProto = "https:ssh:file"
	}
	gitEnv = append(gitEnv, "GIT_ALLOW_PROTOCOL="+allowProto)

	cmd := exec.Command("git", "ls-remote", repoURL, revision)
	// Run from a neutral directory: ls-remote attempts enclosing-repo
	// discovery from the cwd, and a broken .git (e.g. a worktree whose
	// gitdir pointer is outside a container mount) hard-fails it even
	// though the remote URL is explicit.
	cmd.Dir = os.TempDir()
	cmd.Env = gitEnv
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git ls-remote %s %s: %s", redactURL(repoURL), revision, stderr.String())
	}
	parts := strings.Fields(stdout.String())
	if len(parts) < 1 {
		return "", fmt.Errorf("git ls-remote: no output for %s %s", redactURL(repoURL), revision)
	}
	return parts[0], nil
}

// isCommitSHA returns true if s is a 40-character hex string (a pinned commit SHA).
func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// RepoHost extracts the hostname from a validated git repository URL.
// It supports https://, ssh://, and scp-like [user@]host:path formats —
// the same formats accepted by validateRepoURL. Returns an error if the
// URL cannot be parsed (callers should have already validated the URL
// via validateRepoURL before calling this).
func RepoHost(repoURL string) (string, error) {
	u := strings.TrimSpace(repoURL)
	if u == "" {
		return "", fmt.Errorf("repoURL is empty")
	}

	// Scheme form: <scheme>://<host>/<path>
	if i := strings.Index(u, "://"); i >= 0 {
		parsed, err := url.Parse(u)
		if err != nil {
			return "", fmt.Errorf("parse repo URL: %w", err)
		}
		return parsed.Hostname(), nil
	}

	// scp-like shorthand [user@]host:path — split on colon, strip user@ prefix.
	if i := strings.Index(u, ":"); i > 0 && !strings.Contains(u[:i], "/") {
		host := u[:i]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return host, nil
	}

	return "", fmt.Errorf("unrecognized repo URL format: %s", redactURL(u))
}

// CheckHostAllowed verifies that host is present in the comma-separated,
// case-insensitive, exact-match list stored under the AllowedHostsAnnotation
// key. Missing or empty annotation means no hosts are allowed (fail-closed).
// No wildcard support.
func CheckHostAllowed(annotations map[string]string, host string) error {
	allowedStr, ok := annotations[AllowedHostsAnnotation]
	if !ok || strings.TrimSpace(allowedStr) == "" {
		return fmt.Errorf("secret must have annotation %q listing allowed git hosts (basic-auth only)", AllowedHostsAnnotation)
	}

	host = strings.ToLower(strings.TrimSpace(host))
	parts := strings.Split(allowedStr, ",")
	for _, p := range parts {
		if strings.ToLower(strings.TrimSpace(p)) == host {
			return nil
		}
	}
	return fmt.Errorf("git host %q is not in the allowed-hosts list (annotation %q)", host, AllowedHostsAnnotation)
}

// CheckGitSecretHost checks whether the given GitAuth is allowed to
// authenticate against repoURL. It is the single entry point used by all
// git secretRef call sites.
//
// Mechanism exemption: SSH keys (Mechanism() == "ssh") and empty/nil auth
// (Mechanism() == "") are always allowed — SSH keys are not exfiltrated
// during a normal handshake, and empty auth means no credential is being
// sent. Only basic-auth credentials (Mechanism() == "basic") are subject
// to host-scoping via the varroa.dev/allowed-hosts annotation.
func CheckGitSecretHost(auth *GitAuth, annotations map[string]string, repoURL string) error {
	if auth == nil || auth.Mechanism() != "basic" {
		return nil
	}
	host, err := RepoHost(repoURL)
	if err != nil {
		return fmt.Errorf("extract repo host: %w", err)
	}
	return CheckHostAllowed(annotations, host)
}

// ValidateFunc is a function that validates a file's content.
type ValidateFunc func(path string, content []byte) error

// ValidateBundle validates that a bundle directory contains a valid bundle.yaml
// manifest and all referenced files exist.
func ValidateBundle(bundleDir string) error {
	v := NewValidator()
	result := v.Validate(bundleDir)
	if !result.Valid {
		for _, e := range result.Errors {
			return fmt.Errorf("%s", e)
		}
	}
	return nil
}
