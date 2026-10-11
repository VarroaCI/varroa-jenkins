package secretredact

import (
	"strings"
	"testing"
)

func TestItems_RedactsFolderCredentialValues(t *testing.T) {
	in := `items:
  - kind: folder
    name: payments
    properties:
      - folderCredentialsProperty:
          folderCredentials:
            - credentials:
                - usernamePassword:
                    id: user-pass
                    scope: GLOBAL
                    username: admin
                    password: s3cret-pw
                - secretText:
                    id: token
                    secret: "abc${def}"
                - basicSSHUserPrivateKey:
                    id: deploy
                    username: git
                    passphrase: key-pass
                    privateKeySource:
                      directEntry:
                        privateKey: |
                          -----BEGIN FIXTURE KEY-----
                          a2V5LWJvZHk
                          -----END FIXTURE KEY-----
                - file:
                    id: kube-file-id
                    scope: GLOBAL
                    fileName: kube-file-name
                    secretBytes: ZmlsZS1zZWNyZXQ=
                - certificate:
                    id: cert-id
                    scope: GLOBAL
                    password: cert-pass
                    keyStoreSource:
                      uploaded:
                        uploadedKeystore: a2V5c3RvcmUtc2VjcmV0
`
	out := Items(in)
	for _, secret := range []string{"s3cret-pw", "abc${def}", "key-pass", "a2V5LWJvZHk", "ZmlsZS1zZWNyZXQ=", "cert-pass", "a2V5c3RvcmUtc2VjcmV0"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q survived redaction:\n%s", secret, out)
		}
	}
	if got := strings.Count(out, Marker); got != 8 {
		t.Errorf("want 8 markers, got %d:\n%s", got, out)
	}
	for _, visible := range []string{"user-pass", "admin", "username: git", "scope: GLOBAL", "name: payments", "kube-file-id", "cert-id"} {
		if !strings.Contains(out, visible) {
			t.Errorf("non-secret %q is missing:\n%s", visible, out)
		}
	}
}

func TestItems_RedactsWholeReference(t *testing.T) {
	out := Items("password: ${def}\n")
	if strings.Contains(out, "${def}") {
		t.Fatalf("an items value is literal text and must be redacted, got %q", out)
	}
}

func TestJCasC_RedactsSystemCredentials(t *testing.T) {
	in := `credentials:
  system:
    domainCredentials:
      - credentials:
          - string:
              scope: SYSTEM
              id: varroa-cloud-payments
              secret: eyJhbGciOiJSUzI1NiJ9.tenant-token
          - basicSSHUserPrivateKey:
              scope: SYSTEM
              id: varroa-runner-r1
              username: builder
              passphrase: runner-pass
              privateKeySource:
                directEntry:
                  privateKey: runner-key-body
jenkins:
  clouds:
    - kubernetes:
        name: payments
        credentialsId: varroa-cloud-payments
`
	out := JCasC(in)
	for _, secret := range []string{"tenant-token", "runner-pass", "runner-key-body"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q survived redaction:\n%s", secret, out)
		}
	}
	for _, visible := range []string{"varroa-cloud-payments", "varroa-runner-r1", "credentialsId: varroa-cloud-payments", "builder"} {
		if !strings.Contains(out, visible) {
			t.Errorf("non-secret %q is missing:\n%s", visible, out)
		}
	}
}

func TestJCasC_KeepsSecretSourceReference(t *testing.T) {
	in := `credentials:
  system:
    domainCredentials:
      - credentials:
          - aws:
              id: artifact
              secretKey: ${readFile:/var/run/secrets/artifact/secret-access-key}
              accessKey: plain-access
`
	out := JCasC(in)
	if !strings.Contains(out, "secretKey: ${readFile:/var/run/secrets/artifact/secret-access-key}") {
		t.Fatalf("a whole file reference must stay visible, got:\n%s", out)
	}
	if strings.Contains(out, "plain-access") {
		t.Fatalf("an unlisted field in a credential store must be redacted, got:\n%s", out)
	}
}

func TestJCasC_RedactsPartialReference(t *testing.T) {
	out := JCasC("clientSecret: abc${def}\n")
	if strings.Contains(out, "abc") {
		t.Fatalf("a value with text around a reference must be redacted, got %q", out)
	}
}

func TestRedact_KeysThatEndInIDStayVisible(t *testing.T) {
	in := "tokenCredentialId: my-token\ncredentialsId: my-cred\nsecretId: s1\n"
	if out := JCasC(in); out != in {
		t.Fatalf("a key that ends in id names a reference, got:\n%s", out)
	}
}

func TestRedact_EncodesBothSidesTheSameWay(t *testing.T) {
	plain := "jenkins:\n    systemMessage: hello\n\n\n    numExecutors: 0\n"
	withSecret := "jenkins:\n  systemMessage: hello\n  numExecutors: 0\npassword: p\n"
	a, b := JCasC(plain), JCasC(withSecret)
	if !strings.HasPrefix(b, a) {
		t.Fatalf("a side with no secret and a side with a secret must share one style:\n%s\n---\n%s", a, b)
	}
	if again := JCasC(a); again != a {
		t.Fatalf("a second pass must not change the text:\n%s\n---\n%s", a, again)
	}
}

func TestRedact_NestedValueUnderASensitiveKey(t *testing.T) {
	out := JCasC("unclassified:\n  token:\n    value: hunter2\n    list:\n      - value: deep-secret\n")
	if strings.Contains(out, "hunter2") || strings.Contains(out, "deep-secret") {
		t.Fatalf("a value nested under a sensitive key must be redacted, got:\n%s", out)
	}
}

func TestJCasC_KeepsCredentialIDUnderAKeywordKind(t *testing.T) {
	in := `credentials:
  system:
    domainCredentials:
      - credentials:
          - basicSSHUserPrivateKey:
              id: deploy
              scope: GLOBAL
              username: git
`
	out := JCasC(in)
	for _, visible := range []string{"id: deploy", "scope: GLOBAL", "username: git"} {
		if !strings.Contains(out, visible) {
			t.Errorf("%q must stay visible in a store, got:\n%s", visible, out)
		}
	}
}

func TestRedact_SequenceUnderSensitiveKey(t *testing.T) {
	out := Items("tokens:\n  - first-token\n  - second-token\n")
	if strings.Contains(out, "first-token") || strings.Contains(out, "second-token") {
		t.Fatalf("scalar items under a sensitive key must be redacted, got:\n%s", out)
	}
}

func TestRedact_AliasToSecret(t *testing.T) {
	out := Items("base: &pw hunter2\npassword: *pw\n")
	if strings.Contains(out, "hunter2") {
		t.Fatalf("an aliased secret must be redacted at its anchor, got:\n%s", out)
	}
}

func TestRedact_EverySecondDocument(t *testing.T) {
	out := JCasC("jenkins:\n  systemMessage: hi\n---\npassword: in-doc-two\n")
	if strings.Contains(out, "in-doc-two") {
		t.Fatalf("a secret in a later document must be redacted, got:\n%s", out)
	}
	if !strings.Contains(out, "systemMessage: hi") {
		t.Fatalf("the first document must stay, got:\n%s", out)
	}
}

func TestRedact_UnparseableFailsClosed(t *testing.T) {
	in := "password: [unclosed\n  secret: x"
	if out := JCasC(in); out != UnparseableMarker {
		t.Fatalf("unparseable content must not pass through, got %q", out)
	}
}

func TestRedact_EmptyAndNullValuesStay(t *testing.T) {
	in := "password: \"\"\nsecret: null\n"
	if out := Items(in); strings.Contains(out, Marker) {
		t.Fatalf("empty and null values hold no secret, got:\n%s", out)
	}
}

func TestRedact_Idempotent(t *testing.T) {
	once := Items("password: hunter2\n")
	if twice := Items(once); twice != once {
		t.Fatalf("a second pass must not change redacted content:\n%s\n---\n%s", once, twice)
	}
}

func TestIsSensitiveKey(t *testing.T) {
	cases := map[string]bool{
		"password": true, "Password": true, "managerPasswordSecret": true,
		"clientSecret": true, "secret": true, "passphrase": true,
		"privateKey": true, "apiToken": true, "secretBytes": true,
		"credentialsId": false, "tokenCredentialId": false, "id": false,
		"username": false, "description": false, "scope": false,
	}
	for key, want := range cases {
		if got := IsSensitiveKey(key); got != want {
			t.Errorf("IsSensitiveKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestJCasC_RedactsEveryUnknownFieldInACredentialStore(t *testing.T) {
	in := `credentials:
  system:
    domainCredentials:
      - domain:
          name: github
          specifications:
            - hostnameSpecification:
                includes: "*.github.com"
        credentials:
          - certificate:
              id: cert
              scope: GLOBAL
              description: client cert
              keyStoreSource:
                uploaded:
                  uploadedKeystore: cGtjczEyLWJvZHk=
          - kubeconfig:
              id: kube
              kubeconfigSource:
                directEntry:
                  content: kubeconfig-body
`
	out := JCasC(in)
	for _, secret := range []string{"cGtjczEyLWJvZHk=", "kubeconfig-body"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %q survived redaction:\n%s", secret, out)
		}
	}
	for _, visible := range []string{"name: github", "*.github.com", "id: cert", "scope: GLOBAL", "client cert", "id: kube"} {
		if !strings.Contains(out, visible) {
			t.Errorf("non-secret %q is missing:\n%s", visible, out)
		}
	}
}

func TestItems_RedactsEveryUnknownFieldInAFolderStore(t *testing.T) {
	in := `folderCredentials:
  - credentials:
      - file:
          id: f
          fileName: settings.xml
          secretBytes: c2V0dGluZ3M=
          someNewField: unexpected-secret
`
	out := Items(in)
	if strings.Contains(out, "unexpected-secret") || strings.Contains(out, "c2V0dGluZ3M=") {
		t.Fatalf("a folder store field survived redaction:\n%s", out)
	}
}

func TestJCasC_RedactsInlineDataReferences(t *testing.T) {
	for _, v := range []string{
		"${decodeBase64:c2VjcmV0}",
		"${base64:secret}",
		"${json:key:{\"key\":\"v\"}}",
		"${trim: secret }",
		"${PASSWORD:-fallback-secret}",
	} {
		out := JCasC("password: " + quote(v) + "\n")
		if strings.Contains(out, v) {
			t.Errorf("inline-data reference %q must be redacted, got %q", v, out)
		}
	}
}

func TestJCasC_KeepsPointerReferences(t *testing.T) {
	for _, v := range []string{
		"${readFile:/run/secrets/pw}",
		"${file:/run/secrets/pw}",
		"${readFileBase64:/run/secrets/ks}",
		"${sysProp:jenkins.pw}",
		"${GITHUB_TOKEN}",
		"${vault.path.key}",
	} {
		in := "password: " + quote(v) + "\n"
		if out := JCasC(in); !strings.Contains(out, v) {
			t.Errorf("pointer reference %q must stay, got %q", v, out)
		}
	}
}

func quote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func TestJCasC_RedactsAnIDNamedSecretInAStore(t *testing.T) {
	in := `credentials:
  system:
    domainCredentials:
      - credentials:
          - vaultAppRoleCredential:
              id: vault
              roleId: role-123
              secretId: approle-secret-value
`
	out := JCasC(in)
	if strings.Contains(out, "approle-secret-value") || strings.Contains(out, "role-123") {
		t.Fatalf("an id-named field in a credential store must be redacted, got:\n%s", out)
	}
	if !strings.Contains(out, "id: vault") {
		t.Fatalf("the credential id must stay visible, got:\n%s", out)
	}
}

func TestJCasC_RedactsAnAliasedCredentialInAStore(t *testing.T) {
	in := `anchors:
  cert: &cert
    certificate:
      id: cert
      keyStoreSource:
        uploaded:
          uploadedKeystore: cGtjczEyLWJvZHk=
credentials:
  system:
    domainCredentials:
      - credentials:
          - *cert
`
	out := JCasC(in)
	if strings.Contains(out, "cGtjczEyLWJvZHk=") {
		t.Fatalf("an aliased credential in a store must be redacted at its anchor, got:\n%s", out)
	}
}

func TestRedact_RecursiveAnchorTerminates(t *testing.T) {
	in := "a: &x\n  b: *x\n  password: p\n"
	_ = Items(in)
}

func TestRedact_RemovesComments(t *testing.T) {
	in := "# password: header-secret\njenkins:\n  # password: commented-out-secret\n  systemMessage: hi # token: line-secret\n"
	out := JCasC(in)
	for _, secret := range []string{"header-secret", "commented-out-secret", "line-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("a comment with %q must be removed, got:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "systemMessage: hi") {
		t.Errorf("the value must stay, got:\n%s", out)
	}
	if got := Items("# password: only-a-comment\n"); strings.Contains(got, "only-a-comment") {
		t.Errorf("comment-only content must not pass through, got %q", got)
	}
}

func TestRedact_RemovesCommentsInsideAComplexKey(t *testing.T) {
	out := JCasC("? {a: b # hidden-in-key\n  }\n: v\n")
	if strings.Contains(out, "hidden-in-key") {
		t.Fatalf("a comment inside a complex key must be removed, got:\n%s", out)
	}
}
