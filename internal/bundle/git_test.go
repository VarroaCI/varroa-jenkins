package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitAuth_IsEmpty(t *testing.T) {
	tests := []struct {
		name string
		auth *GitAuth
		want bool
	}{
		{"nil", nil, true},
		{"empty", &GitAuth{}, true},
		{"basic", &GitAuth{Username: "u", Password: "p"}, false},
		{"ssh", &GitAuth{SSHPrivateKey: "key"}, false},
		{"username only", &GitAuth{Username: "u"}, false},
		{"password only", &GitAuth{Password: "p"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.IsEmpty(); got != tt.want {
				t.Errorf("IsEmpty() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGitAuth_Mechanism(t *testing.T) {
	tests := []struct {
		name string
		auth *GitAuth
		want string
	}{
		{"nil", nil, ""},
		{"empty", &GitAuth{}, ""},
		{"basic", &GitAuth{Username: "u", Password: "p"}, "basic"},
		{"ssh", &GitAuth{SSHPrivateKey: "key"}, "ssh"},
		{"username only", &GitAuth{Username: "u"}, "basic"},
		{"password only", &GitAuth{Password: "p"}, "basic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.auth.Mechanism(); got != tt.want {
				t.Errorf("Mechanism() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitAuthFromSecret(t *testing.T) {
	tests := []struct {
		name       string
		data       map[string][]byte
		secretType string
		wantErr    string
		wantUser   string
		wantPass   string
		wantKey    string
	}{
		{
			name:       "basic-auth",
			data:       map[string][]byte{"username": []byte("alice"), "password": []byte("s3cret")},
			secretType: "kubernetes.io/basic-auth",
			wantUser:   "alice",
			wantPass:   "s3cret",
		},
		{
			name:       "basic-auth with token",
			data:       map[string][]byte{"username": []byte("bot"), "token": []byte("tok")},
			secretType: "kubernetes.io/basic-auth",
			wantUser:   "bot",
			wantPass:   "tok",
		},
		{
			name:       "ssh-auth",
			data:       map[string][]byte{"ssh-privatekey": []byte("rsa-key-data")},
			secretType: "kubernetes.io/ssh-auth",
			wantKey:    "rsa-key-data",
		},
		{
			name:       "empty secret",
			data:       map[string][]byte{},
			secretType: "kubernetes.io/basic-auth",
			wantErr:    "secret is empty",
		},
		{
			name:       "basic-auth missing credentials",
			data:       map[string][]byte{"username": []byte("")},
			secretType: "kubernetes.io/basic-auth",
			wantErr:    "basic-auth secret missing username and password/token keys",
		},
		{
			name:       "ssh-auth missing key",
			data:       map[string][]byte{"other": []byte("x")},
			secretType: "kubernetes.io/ssh-auth",
			wantErr:    "ssh-auth secret missing ssh-privatekey key",
		},
		{
			name:       "unsupported type",
			data:       map[string][]byte{"x": []byte("y")},
			secretType: "unknown",
			wantErr:    "unsupported secret type",
		},
		{
			name:       "fallback basic from opaque",
			data:       map[string][]byte{"username": []byte("u"), "password": []byte("p")},
			secretType: "Opaque",
			wantUser:   "u",
			wantPass:   "p",
		},
		{
			name:       "fallback ssh from opaque",
			data:       map[string][]byte{"ssh-privatekey": []byte("k")},
			secretType: "Opaque",
			wantKey:    "k",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth, err := GitAuthFromSecret(tt.data, tt.secretType)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want containing %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantUser != "" && auth.Username != tt.wantUser {
				t.Errorf("Username = %q, want %q", auth.Username, tt.wantUser)
			}
			if tt.wantPass != "" && auth.Password != tt.wantPass {
				t.Errorf("Password = %q, want %q", auth.Password, tt.wantPass)
			}
			if tt.wantKey != "" && auth.SSHPrivateKey != tt.wantKey {
				t.Errorf("SSHPrivateKey = %q, want %q", auth.SSHPrivateKey, tt.wantKey)
			}
			// Errors must never contain secret material.
			if err != nil {
				for _, v := range tt.data {
					if strings.Contains(err.Error(), string(v)) && string(v) != "" {
						t.Errorf("error message contains secret data: %q", err.Error())
					}
				}
			}
		})
	}
}

func TestGitAuthEnv_BasicAuth(t *testing.T) {
	auth := &GitAuth{Username: "testuser", Password: "testpass"}
	env, cleanup, err := gitAuthEnv(auth)
	if err != nil {
		t.Fatalf("gitAuthEnv error: %v", err)
	}
	defer cleanup()

	// Verify GIT_ASKPASS is set and points to an executable script.
	foundAskpass := false
	foundTerminal := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_ASKPASS=") {
			foundAskpass = true
			path := strings.TrimPrefix(e, "GIT_ASKPASS=")
			// Verify the askpass script exists and is executable.
			fi, err := os.Stat(path)
			if err != nil {
				t.Errorf("askpass script not found: %v", err)
			} else if fi.Mode()&0100 == 0 {
				t.Error("askpass script is not executable")
			}
			// Read and verify the script content contains the password.
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("cannot read askpass script: %v", err)
			} else if !strings.Contains(string(data), "testpass") {
				t.Error("askpass script does not contain password")
			}
		}
		if e == "GIT_TERMINAL_PROMPT=0" {
			foundTerminal = true
		}
	}
	if !foundAskpass {
		t.Error("GIT_ASKPASS not set in env")
	}
	if !foundTerminal {
		t.Error("GIT_TERMINAL_PROMPT=0 not set")
	}
}

func TestGitAuthEnv_SSH(t *testing.T) {
	auth := &GitAuth{SSHPrivateKey: "fake-ssh-key\n"}
	env, cleanup, err := gitAuthEnv(auth)
	if err != nil {
		t.Fatalf("gitAuthEnv error: %v", err)
	}
	defer cleanup()

	foundSSH := false
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			foundSSH = true
			cmd := strings.TrimPrefix(e, "GIT_SSH_COMMAND=")
			if !strings.Contains(cmd, "-i ") {
				t.Error("GIT_SSH_COMMAND missing -i flag")
			}
			if !strings.Contains(cmd, "StrictHostKeyChecking=accept-new") {
				t.Error("GIT_SSH_COMMAND missing StrictHostKeyChecking")
			}
			// Extract the key path and verify the file exists with 0600 perms.
			parts := strings.Fields(cmd)
			for i, p := range parts {
				if p == "-i" && i+1 < len(parts) {
					keyPath := parts[i+1]
					fi, err := os.Stat(keyPath)
					if err != nil {
						t.Errorf("SSH key file not found at %s: %v", keyPath, err)
					} else if fi.Mode().Perm() != 0600 {
						t.Errorf("SSH key has permissions %o, want 0600", fi.Mode().Perm())
					}
					data, _ := os.ReadFile(keyPath)
					if string(data) != "fake-ssh-key\n" {
						t.Errorf("SSH key content = %q, want 'fake-ssh-key\\n'", string(data))
					}
				}
			}
		}
	}
	if !foundSSH {
		t.Error("GIT_SSH_COMMAND not set in env")
	}

	// Verify cleanup removes the key file.
	cleanup()
	// After cleanup, the key file should be gone.
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_SSH_COMMAND=") {
			cmd := strings.TrimPrefix(e, "GIT_SSH_COMMAND=")
			parts := strings.Fields(cmd)
			for i, p := range parts {
				if p == "-i" && i+1 < len(parts) {
					keyPath := parts[i+1]
					if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
						t.Error("SSH key file not removed by cleanup")
					}
				}
			}
		}
	}
}

func TestGitAuthEnv_Nil(t *testing.T) {
	env, cleanup, err := gitAuthEnv(nil)
	if err != nil {
		t.Fatalf("gitAuthEnv(nil) error: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("expected empty env for nil auth, got %v", env)
	}
	cleanup() // should be a no-op
}

func TestGitAuthEnv_Empty(t *testing.T) {
	env, cleanup, err := gitAuthEnv(&GitAuth{})
	if err != nil {
		t.Fatalf("gitAuthEnv(empty) error: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("expected empty env for empty auth, got %v", env)
	}
	cleanup()
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"https://example.com/repo.git", "https://example.com/repo.git"},
		{"https://user:pass@example.com/repo.git", "https://***@example.com/repo.git"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"https://token@github.com/org/repo.git", "https://***@github.com/org/repo.git"},
		{"http://user:password@host.xz/path", "http://***@host.xz/path"},
		{"plain-string", "plain-string"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got := redactURL(tt.raw)
			if got != tt.want {
				t.Errorf("redactURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
			// Redacted output must never contain the password portion.
			if strings.Contains(got, "pass") && tt.want != tt.raw {
				t.Errorf("redacted URL still contains credentials: %q", got)
			}
		})
	}
}

func TestCloneRejectsFileProtocol(t *testing.T) {
	// file:// is a local-file-read / transport vector: Clone must reject it up front,
	// before touching the filesystem or invoking git. (GIT_ALLOW_PROTOCOL=https:ssh
	// is a second line of defense that would also block it.)
	fixtureDir := t.TempDir()
	bareRepo := filepath.Join(fixtureDir, "bare.git")

	cloner := NewGitCloner()
	cloneDir := filepath.Join(fixtureDir, "clone-target")
	err := cloner.Clone("file://"+bareRepo, "0000000000000000000000000000000000000000", cloneDir, nil)
	if err == nil {
		t.Fatal("Clone with file:// protocol should be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "disallowed scheme") {
		t.Errorf("expected disallowed-scheme error, got: %v", err)
	}
	if _, statErr := os.Stat(cloneDir); statErr == nil {
		t.Error("Clone should reject before creating the target dir")
	}
}

func TestValidateRepoURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https", "https://github.com/org/repo.git", false},
		{"ssh scheme", "ssh://git@github.com/org/repo.git", false},
		{"scp-like git@", "git@github.com:org/repo.git", false},
		{"scp-like other user", "deploy@host.example.com:org/repo.git", false},
		{"https with ipv6 literal", "https://[::1]/repo.git", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"http rejected", "http://x/repo.git", true},
		{"git scheme rejected", "git://x/repo.git", true},
		{"unknown scheme rejected", "foo://x/repo.git", true},
		{"file rejected", "file:///etc/passwd", true},
		{"ext transport helper rejected", "ext::sh -c 'touch /tmp/pwned'", true},
		{"fd transport helper rejected", "fd::17/foo", true},
		{"bare local path rejected", "/tmp/some/repo.git", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRepoURL(tt.url, false)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRepoURL(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

func TestRemoteSHA_ReturnsPinnedSHA(t *testing.T) {
	cloner := NewGitCloner()
	pinned := "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"
	sha, err := cloner.RemoteSHA("https://example.com/repo.git", pinned, nil)
	if err != nil {
		t.Fatalf("RemoteSHA with pinned SHA: %v", err)
	}
	if sha != pinned {
		t.Errorf("RemoteSHA returned %q, want pinned SHA %q", sha, pinned)
	}
}

func TestRemoteSHA_EmptyRevision(t *testing.T) {
	cloner := NewGitCloner()
	_, err := cloner.RemoteSHA("https://example.com/repo.git", "", nil)
	if err == nil {
		t.Error("expected error for empty revision")
	}
}

func TestIsCommitSHA(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", true},
		{"A1B2C3D4E5F6A7B8C9D0E1F2A3B4C5D6E7F8A9B0", true},
		{"short", false},
		{"a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0extra", false},
		{"g1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			if got := isCommitSHA(tt.s); got != tt.want {
				t.Errorf("isCommitSHA(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestRepoHost(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"https github", "https://github.com/org/repo.git", "github.com", false},
		{"ssh scheme", "ssh://git@github.com/org/repo.git", "github.com", false},
		{"scp-like git@", "git@github.com:org/repo.git", "github.com", false},
		{"scp-like deploy@", "deploy@host.example.com:org/repo.git", "host.example.com", false},
		{"https ipv6 literal", "https://[::1]/repo.git", "::1", false},
		{"https with port", "https://gitlab.com:8443/group/repo.git", "gitlab.com", false},
		{"empty", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RepoHost(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("RepoHost(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("RepoHost(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestCheckHostAllowed(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		host        string
		wantErr     bool
	}{
		{
			name:        "missing annotation",
			annotations: map[string]string{},
			host:        "github.com",
			wantErr:     true,
		},
		{
			name:        "empty annotation value",
			annotations: map[string]string{AllowedHostsAnnotation: ""},
			host:        "github.com",
			wantErr:     true,
		},
		{
			name:        "single entry match",
			annotations: map[string]string{AllowedHostsAnnotation: "github.com"},
			host:        "github.com",
			wantErr:     false,
		},
		{
			name:        "multi-entry comma-separated match",
			annotations: map[string]string{AllowedHostsAnnotation: "gitlab.com, github.com, bitbucket.org"},
			host:        "github.com",
			wantErr:     false,
		},
		{
			name:        "case-insensitive match",
			annotations: map[string]string{AllowedHostsAnnotation: "GITHUB.COM"},
			host:        "GitHub.com",
			wantErr:     false,
		},
		{
			name:        "host not in list",
			annotations: map[string]string{AllowedHostsAnnotation: "github.com"},
			host:        "attacker.example.com",
			wantErr:     true,
		},
		{
			name:        "nil annotations",
			annotations: nil,
			host:        "github.com",
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckHostAllowed(tt.annotations, tt.host)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckHostAllowed(%v, %q) error = %v, wantErr %v", tt.annotations, tt.host, err, tt.wantErr)
			}
		})
	}
}

func TestCheckGitSecretHost(t *testing.T) {
	tests := []struct {
		name        string
		auth        *GitAuth
		annotations map[string]string
		repoURL     string
		wantErr     bool
	}{
		{
			name:        "ssh exempt - no annotation",
			auth:        &GitAuth{SSHPrivateKey: "key"},
			annotations: nil,
			repoURL:     "git@attacker.example:x.git",
			wantErr:     false,
		},
		{
			name:        "empty auth exempt",
			auth:        &GitAuth{},
			annotations: nil,
			repoURL:     "https://attacker.example/x.git",
			wantErr:     false,
		},
		{
			name:        "nil auth exempt",
			auth:        nil,
			annotations: nil,
			repoURL:     "https://attacker.example/x.git",
			wantErr:     false,
		},
		{
			name:        "basic - no annotation fails closed",
			auth:        &GitAuth{Username: "u", Password: "p"},
			annotations: nil,
			repoURL:     "https://github.com/org/repo.git",
			wantErr:     true,
		},
		{
			name:        "basic - matching annotation passes",
			auth:        &GitAuth{Username: "u", Password: "p"},
			annotations: map[string]string{AllowedHostsAnnotation: "github.com"},
			repoURL:     "https://github.com/org/repo.git",
			wantErr:     false,
		},
		{
			name:        "basic - non-matching annotation fails",
			auth:        &GitAuth{Username: "u", Password: "p"},
			annotations: map[string]string{AllowedHostsAnnotation: "gitlab.com"},
			repoURL:     "https://github.com/org/repo.git",
			wantErr:     true,
		},
		{
			name:        "basic with scp URL and matching annotation",
			auth:        &GitAuth{Username: "git", Password: "token"},
			annotations: map[string]string{AllowedHostsAnnotation: "github.com"},
			repoURL:     "git@github.com:org/repo.git",
			wantErr:     false,
		},
		{
			name:        "basic with ssh scheme and matching annotation",
			auth:        &GitAuth{Username: "git", Password: "token"},
			annotations: map[string]string{AllowedHostsAnnotation: "github.com"},
			repoURL:     "ssh://git@github.com/org/repo.git",
			wantErr:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckGitSecretHost(tt.auth, tt.annotations, tt.repoURL)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckGitSecretHost(%v, %v, %q) error = %v, wantErr %v", tt.auth, tt.annotations, tt.repoURL, err, tt.wantErr)
			}
		})
	}
}

// Helpers
