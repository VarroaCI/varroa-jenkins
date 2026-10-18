package bundle

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
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

// Clone fetches the commit at revision into a temporary bare repository and
// writes its tree to targetDir with git archive, so .gitattributes export
// rules apply. The target receives the files of the tree and no .git
// directory. It returns the resolved commit SHA, peeled for an annotated tag.
// targetDir must be a missing
// path or an empty directory. revision may be empty (HEAD), a branch or tag, or a
// full commit SHA. auth may be nil for public repositories. Every git
// process stops when ctx is done, and the returned error then wraps ctx.Err().
func (g *GitCloner) Clone(ctx context.Context, repoURL, revision, targetDir string, auth *GitAuth) (string, error) {
	if err := validateRepoURL(repoURL, g.allowLocalTransport); err != nil {
		return "", err
	}
	sha, err := g.resolveRevision(ctx, repoURL, revision, auth)
	if err != nil {
		return "", err
	}

	bare, err := os.MkdirTemp("", "varroa-git-bare-*")
	if err != nil {
		return "", fmt.Errorf("create temp bare repo: %w", err)
	}
	defer func() { _ = os.RemoveAll(bare) }()

	if err := initBareRepo(ctx, bare); err != nil {
		return "", err
	}
	sha, err = fetchCommit(ctx, bare, repoURL, sha, revision, auth, g.allowLocalTransport)
	if err != nil {
		return "", err
	}
	if err := archiveCommit(ctx, bare, sha, targetDir); err != nil {
		return "", err
	}
	return sha, nil
}

// resolveRevision returns the commit SHA for revision. A full commit SHA is
// returned as it is, so no remote call runs. An empty revision resolves HEAD.
func (g *GitCloner) resolveRevision(ctx context.Context, repoURL, revision string, auth *GitAuth) (string, error) {
	if isCommitSHA(revision) {
		return revision, nil
	}
	if revision == "" {
		sha, err := g.RemoteSHA(ctx, repoURL, "HEAD", auth)
		if err != nil {
			return "", fmt.Errorf("resolve HEAD: %w", err)
		}
		return sha, nil
	}
	sha, err := g.RemoteSHA(ctx, repoURL, revision, auth)
	if err != nil {
		return "", fmt.Errorf("resolve revision %s: %w", revision, err)
	}
	return sha, nil
}

// remoteGitEnv returns the environment for a git process that talks to a
// remote. It holds the auth overrides, disables prompting, and allows only
// the permitted transports. The cleanup function removes the auth temp files.
func remoteGitEnv(auth *GitAuth, allowLocal bool) ([]string, func(), error) {
	authEnv, cleanup, err := gitAuthEnv(auth)
	if err != nil {
		return nil, func() {}, fmt.Errorf("setup git auth: %w", err)
	}
	allowProto := "https:ssh"
	if allowLocal {
		allowProto = "https:ssh:file"
	}
	env := append(os.Environ(), authEnv...)
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL="+allowProto)
	return env, cleanup, nil
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

// RemoteSHA returns the commit SHA for a remote branch or tag with git
// ls-remote. For an annotated tag it returns the commit that the tag points
// to. A 40-hex revision is returned as it is, without a remote call, and it
// can be a tag object SHA. auth may be nil for public repositories. The git
// process stops when ctx is done, and the returned error then wraps ctx.Err().
func (g *GitCloner) RemoteSHA(ctx context.Context, repoURL, revision string, auth *GitAuth) (string, error) {
	if err := validateRepoURL(repoURL, g.allowLocalTransport); err != nil {
		return "", err
	}
	if revision == "" {
		return "", fmt.Errorf("revision is required for git ls-remote")
	}
	if isCommitSHA(revision) {
		return revision, nil
	}

	gitEnv, cleanup, err := remoteGitEnv(auth, g.allowLocalTransport)
	if err != nil {
		return "", err
	}
	defer cleanup()

	cmd := gitCommand(ctx, "ls-remote", repoURL, revision, revision+"^{}")
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
		if ctx.Err() != nil {
			return "", fmt.Errorf("git ls-remote %s %s: %w", redactURL(repoURL), revision, ctx.Err())
		}
		return "", fmt.Errorf("git ls-remote %s %s: %s", redactURL(repoURL), revision, stderr.String())
	}
	sha, ok := lsRemoteSHA(stdout.String())
	if !ok {
		return "", fmt.Errorf("git ls-remote: no output for %s %s", redactURL(repoURL), revision)
	}
	return sha, nil
}

// lsRemoteSHA returns the SHA of the first ref in ls-remote output. When the
// output also holds the peeled line "<ref>^{}" for that ref, it returns the
// peeled SHA, which is the commit that an annotated tag points to.
func lsRemoteSHA(out string) (string, bool) {
	var firstSHA, firstRef string
	peeled := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if ref, ok := strings.CutSuffix(f[1], "^{}"); ok {
			peeled[ref] = f[0]
			continue
		}
		if firstRef == "" {
			firstSHA, firstRef = f[0], f[1]
		}
	}
	if firstRef == "" {
		return "", false
	}
	if p, ok := peeled[firstRef]; ok {
		return p, true
	}
	return firstSHA, true
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
