package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/overlay"
)

// fakeArtifactIssuer records each request and answers with a numbered
// credential, or with err when it is set.
type fakeArtifactIssuer struct {
	requests []artifactIssueRequest
	err      error
	now      func() time.Time
}

func (f *fakeArtifactIssuer) Issue(_ context.Context, req artifactIssueRequest) (artifactSessionCredential, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return artifactSessionCredential{}, f.err
	}
	n := len(f.requests)
	return artifactSessionCredential{
		AccessKeyID:     fmt.Sprintf("ASIA%d", n),
		SecretAccessKey: fmt.Sprintf("secret-%d", n),
		SessionToken:    fmt.Sprintf("token-%d", n),
		Expiration:      f.now().Add(req.Duration),
	}, nil
}

// stsTestReconciler builds a reconciler in STS mode with an issuer Secret, a
// fake issuer, and a settable clock.
func stsTestReconciler(t *testing.T) (*Reconciler, *testClient, *fakeArtifactIssuer, *time.Time) {
	t.Helper()
	client := newTestClientWithBundle()
	client.existingSecrets = map[string]map[string][]byte{
		"varroa-artifact-issuer": {
			"access-key-id":     []byte("AKIAISSUER"),
			"secret-access-key": []byte("issuersecret"),
		},
	}
	rec := newTestReconciler(client)
	rec.SetClusterName("core")
	rec.SetClaimGate("1")
	rec.SetBlobstoreURL("s3://varroa-blobs?region=us-east-1&endpoint=http://minio.example:9000&s3ForcePathStyle=true")
	rec.SetArtifactSTS(ArtifactSTSConfig{
		IssuerSecret: "varroa-artifact-issuer",
		RoleARN:      "arn:minio:iam:::role/varroa-artifacts",
		Duration:     time.Hour,
	})
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec.artifactNow = func() time.Time { return clock }
	issuer := &fakeArtifactIssuer{now: func() time.Time { return clock }}
	rec.artifactIssuer = issuer
	return rec, client, issuer, &clock
}

func TestParseArtifactSTSConfig(t *testing.T) {
	cases := []struct {
		name                       string
		issuer, role, ep, duration string
		wantErr                    bool
		want                       time.Duration
	}{
		{name: "disabled", want: 0},
		{name: "default duration", issuer: "i", role: "arn:x", want: time.Hour},
		{name: "floor", issuer: "i", role: "arn:x", duration: "15m", want: 15 * time.Minute},
		{name: "under floor", issuer: "i", role: "arn:x", duration: "14m", wantErr: true},
		{name: "over ceiling", issuer: "i", role: "arn:x", duration: "13h", wantErr: true},
		{name: "bad duration", issuer: "i", role: "arn:x", duration: "soon", wantErr: true},
		{name: "no role", issuer: "i", wantErr: true},
		{name: "bad endpoint", issuer: "i", role: "arn:x", ep: "minio:9000", wantErr: true},
		{name: "good endpoint", issuer: "i", role: "arn:x", ep: "http://minio:9000", want: time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseArtifactSTSConfig(tc.issuer, tc.role, tc.ep, tc.duration)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if cfg.Duration != tc.want {
				t.Errorf("duration = %s, want %s", cfg.Duration, tc.want)
			}
		})
	}
}

func TestArtifactSessionPolicy_ScopesToCellPrefix(t *testing.T) {
	prefix := "artifacts/core/ns/cell-big-0/"
	raw := artifactSessionPolicy("varroa-blobs", prefix)
	var doc artifactPolicyDocument
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("policy is not JSON: %v", err)
	}
	if len(doc.Statement) != 3 {
		t.Fatalf("statements = %d, want 3", len(doc.Statement))
	}
	list := doc.Statement[0]
	if list.Action[0] != "s3:ListBucket" || list.Condition["StringLike"]["s3:prefix"] != prefix+"*" {
		t.Errorf("ListBucket statement = %+v, want a prefix condition", list)
	}
	objects := doc.Statement[2]
	if objects.Resource[0] != "arn:aws:s3:::varroa-blobs/"+prefix+"*" {
		t.Errorf("object resource = %v", objects.Resource)
	}
	for _, s := range doc.Statement {
		for _, a := range s.Action {
			if strings.HasPrefix(a, "sts:") || a == "s3:CreateBucket" || a == "s3:*" {
				t.Errorf("policy grants %s", a)
			}
		}
	}
	if strings.Contains(raw, "logs/") {
		t.Error("policy must not name the logs prefix")
	}
	if len(raw) > 2048 {
		t.Errorf("policy is %d bytes; STS refuses more than 2048", len(raw))
	}
}

func TestArtifactSessionName(t *testing.T) {
	if got := artifactSessionName("core", "team_a", "cell-big-0"); got != "varroa-core-team-a-cell-big-0" {
		t.Errorf("session name = %q", got)
	}
	long1 := artifactSessionName("core", strings.Repeat("n", 40), "cell-"+strings.Repeat("a", 30)+"-1")
	long2 := artifactSessionName("core", strings.Repeat("n", 40), "cell-"+strings.Repeat("a", 30)+"-2")
	if len(long1) > 64 || len(long2) > 64 {
		t.Errorf("session names exceed 64 characters: %d, %d", len(long1), len(long2))
	}
	if long1 == long2 {
		t.Error("two long names must not share one session name")
	}
}

func TestRenderSharedCredentials(t *testing.T) {
	got := string(renderSharedCredentials(artifactSessionCredential{AccessKeyID: "A", SecretAccessKey: "S", SessionToken: "T"}))
	want := "[default]\naws_access_key_id = A\naws_secret_access_key = S\naws_session_token = T\n"
	if got != want {
		t.Errorf("credentials file = %q, want %q", got, want)
	}
}

func TestArtifactSTSEndpoint(t *testing.T) {
	rec := &Reconciler{}
	if got := rec.artifactSTSEndpoint(blobstoreParams{CustomEndpoint: "minio:9000", UseHTTP: true}); got != "http://minio:9000" {
		t.Errorf("custom endpoint = %q", got)
	}
	if got := rec.artifactSTSEndpoint(blobstoreParams{}); got != "" {
		t.Errorf("AWS default = %q, want empty", got)
	}
	rec.artifactSTS.Endpoint = "https://sts.example"
	if got := rec.artifactSTSEndpoint(blobstoreParams{CustomEndpoint: "minio:9000"}); got != "https://sts.example" {
		t.Errorf("override = %q", got)
	}
}

// TestSTSIssuer_AssumeRoleRequest runs the real SDK client against a local
// endpoint and checks the request the operator sends.
func TestSTSIssuer_AssumeRoleRequest(t *testing.T) {
	var form url.Values
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
<AssumeRoleResult><Credentials>
<AccessKeyId>ASIATEMP</AccessKeyId><SecretAccessKey>tempsecret</SecretAccessKey>
<SessionToken>temptoken</SessionToken><Expiration>2026-10-01T13:00:00Z</Expiration>
</Credentials></AssumeRoleResult>
<ResponseMetadata><RequestId>r1</RequestId></ResponseMetadata></AssumeRoleResponse>`)
	}))
	defer srv.Close()

	cred, err := stsArtifactIssuer{httpClient: srv.Client()}.Issue(context.Background(), artifactIssueRequest{
		AccessKeyID:     "AKIAISSUER",
		SecretAccessKey: "issuersecret",
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		RoleARN:         "arn:minio:iam:::role/varroa-artifacts",
		SessionName:     "varroa-core-ns-cell",
		Policy:          `{"Version":"2012-10-17"}`,
		Duration:        time.Hour,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if cred.AccessKeyID != "ASIATEMP" || cred.SessionToken != "temptoken" || !cred.Expiration.Equal(time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("credential = %+v", cred)
	}
	wants := map[string]string{
		"Action":          "AssumeRole",
		"RoleArn":         "arn:minio:iam:::role/varroa-artifacts",
		"RoleSessionName": "varroa-core-ns-cell",
		"Policy":          `{"Version":"2012-10-17"}`,
		"DurationSeconds": "3600",
	}
	for k, v := range wants {
		if form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, form.Get(k), v)
		}
	}
	if !strings.Contains(auth, "Credential=AKIAISSUER/") || !strings.Contains(auth, "/sts/aws4_request") {
		t.Errorf("Authorization = %q, want a SigV4 sts signature with the issuer key", auth)
	}
}

func TestSTSIssuer_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = io.WriteString(w, `<ErrorResponse><Error><Code>NotImplemented</Code><Message>no STS</Message></Error></ErrorResponse>`)
	}))
	defer srv.Close()
	_, err := stsArtifactIssuer{httpClient: srv.Client()}.Issue(context.Background(), artifactIssueRequest{
		AccessKeyID: "a", SecretAccessKey: "b", Endpoint: srv.URL, Region: "us-east-1",
		RoleARN: "arn:x", SessionName: "s", Policy: "{}", Duration: time.Hour,
	})
	if err == nil || !strings.Contains(err.Error(), "NotImplemented") {
		t.Fatalf("err = %v, want the store's NotImplemented error", err)
	}
}

func TestResolveArtifactSecret_STSIssuesAndProjects(t *testing.T) {
	rec, client, issuer, _ := stsTestReconciler(t)
	cr := artifactCellController("cell-big-0", "big")

	name, hash, err := rec.resolveArtifactSecret(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveArtifactSecret: %v", err)
	}
	if name != "cell-big-0-artifacts" || hash != "" {
		t.Fatalf("name=%q hash=%q, want the projected name and no hash", name, hash)
	}
	if len(issuer.requests) != 1 {
		t.Fatalf("issue calls = %d, want 1", len(issuer.requests))
	}
	req := issuer.requests[0]
	if req.AccessKeyID != "AKIAISSUER" || req.Endpoint != "http://minio.example:9000" || req.Region != "us-east-1" {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(req.Policy, "varroa-blobs/artifacts/core/ns/cell-big-0/*") {
		t.Errorf("policy = %s, want the cell prefix", req.Policy)
	}
	data := client.createdSecrets[name]
	if !strings.Contains(string(data[artifactCredentialsKey]), "aws_session_token = token-1") {
		t.Errorf("credentials = %q", data[artifactCredentialsKey])
	}
	if string(data[artifactIssuedAtKey]) != "2026-10-01T12:00:00Z" || string(data[artifactExpirationKey]) != "2026-10-01T13:00:00Z" {
		t.Errorf("times = %q / %q", data[artifactIssuedAtKey], data[artifactExpirationKey])
	}
	if _, ok := data["access-key-id"]; ok {
		t.Error("STS copy must not carry the static key names")
	}
	if owner := client.secretOwners[name]; owner.Kind != "Controller" || owner.Name != cr.Name {
		t.Errorf("owner = %+v", owner)
	}
}

func TestResolveArtifactSecret_STSRenewsAtHalfLife(t *testing.T) {
	rec, _, issuer, clock := stsTestReconciler(t)
	cr := artifactCellController("cell-big-0", "big")

	for _, step := range []struct {
		advance   time.Duration
		wantCalls int
	}{
		{0, 1},
		{10 * time.Minute, 1},
		{19 * time.Minute, 1},
		{2 * time.Minute, 2},
		{5 * time.Minute, 2},
	} {
		*clock = clock.Add(step.advance)
		if _, _, err := rec.resolveArtifactSecret(context.Background(), cr); err != nil {
			t.Fatalf("resolveArtifactSecret: %v", err)
		}
		if len(issuer.requests) != step.wantCalls {
			t.Fatalf("after %s: issue calls = %d, want %d", step.advance, len(issuer.requests), step.wantCalls)
		}
	}
}

func TestResolveArtifactSecret_STSSkipsNonCellAndMissingIssuer(t *testing.T) {
	rec, client, issuer, _ := stsTestReconciler(t)
	name, _, err := rec.resolveArtifactSecret(context.Background(), testController("ci", "ns", v1alpha1.ControllerPhaseProvisioning))
	if err != nil || name != "" {
		t.Fatalf("non-cell: name=%q err=%v, want no projection", name, err)
	}
	delete(client.existingSecrets, "varroa-artifact-issuer")
	name, _, err = rec.resolveArtifactSecret(context.Background(), artifactCellController("cell-big-0", "big"))
	if err != nil || name != "" {
		t.Fatalf("missing issuer: name=%q err=%v, want no projection", name, err)
	}
	if len(issuer.requests) != 0 {
		t.Errorf("issue calls = %d, want 0", len(issuer.requests))
	}
}

func TestResolveArtifactSecret_STSIssueFailureReturnsError(t *testing.T) {
	rec, _, issuer, _ := stsTestReconciler(t)
	issuer.err = errors.New("NotImplemented: no STS")
	_, _, err := rec.resolveArtifactSecret(context.Background(), artifactCellController("cell-big-0", "big"))
	if err == nil || !strings.Contains(err.Error(), "NotImplemented") {
		t.Fatalf("err = %v, want the issue error", err)
	}
}

func fragmentValue(t *testing.T, fragment string, path ...string) any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(fragment), &doc); err != nil {
		t.Fatalf("unmarshal fragment: %v", err)
	}
	cur := any(doc)
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

func TestArtifactStorageFragment_STSShape(t *testing.T) {
	rec, client, _, _ := stsTestReconciler(t)
	cr := artifactCellController("cell-big-0", "big")
	pinnedProfile(client, cr)
	seedClientCRDs(client)

	fragment, cond, err := rec.artifactStorageFragment(context.Background(), cr)
	if err != nil || fragment == "" {
		t.Fatalf("fragment=%q err=%v, want a fragment", fragment, err)
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != "External" || !strings.Contains(cond.Message, `issuer Secret "varroa-artifact-issuer"`) {
		t.Errorf("condition = %+v, want True/External naming the issuer", cond)
	}
	if got := fragmentValue(t, fragment, "aws", "awsCredentials", "credentialsId"); got != "" {
		t.Errorf("credentialsId = %v, want empty so the plugin reads the SDK default chain", got)
	}
	if got := fragmentValue(t, fragment, "aws", "s3", "disableSessionToken"); got != false {
		t.Errorf("disableSessionToken = %v, want false", got)
	}

	merged, _, err := rec.applyArtifactStorage(context.Background(), cr, "jenkins:\n  systemMessage: hi\n")
	if err != nil {
		t.Fatalf("applyArtifactStorage: %v", err)
	}
	if strings.Contains(merged, artifactCredentialID) || strings.Contains(merged, "readFile") {
		t.Errorf("STS mode must inject no credential, got:\n%s", merged)
	}
}

func TestArtifactStorageFragment_STSLocalOnlyCases(t *testing.T) {
	t.Run("non-cell", func(t *testing.T) {
		rec, _, _, _ := stsTestReconciler(t)
		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), testController("ci", "ns", v1alpha1.ControllerPhaseProvisioning))
		if fragment != "" || cond.Reason != "LocalOnly" || cond.Message != "the operator issues STS artifact credentials to cells only" {
			t.Errorf("fragment=%q cond=%+v", fragment, cond)
		}
	})
	t.Run("issuer missing", func(t *testing.T) {
		rec, client, _, _ := stsTestReconciler(t)
		client.getSecretErrs = map[string]error{
			"varroa-artifact-issuer": apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "varroa-artifact-issuer"),
		}
		fragment, cond, err := rec.artifactStorageFragment(context.Background(), artifactCellController("cell-big-0", "big"))
		if err != nil || fragment != "" || cond.Reason != "LocalOnly" || cond.Message != `artifact issuer secret "varroa-artifact-issuer" not found` {
			t.Errorf("fragment=%q cond=%+v err=%v", fragment, cond, err)
		}
	})
	t.Run("issuer read fault", func(t *testing.T) {
		rec, client, _, _ := stsTestReconciler(t)
		client.getSecretErrs = map[string]error{"varroa-artifact-issuer": errors.New("etcd unavailable")}
		_, cond, err := rec.artifactStorageFragment(context.Background(), artifactCellController("cell-big-0", "big"))
		if !errors.Is(err, errArtifactSecretRead) || cond.Reason != "LocalOnly" {
			t.Errorf("cond=%+v err=%v, want a wrapped read fault", cond, err)
		}
	})
}

// TestRenewArtifactCredential_FailureKeepsCopyAndReportsIssueFailed covers
// the renewal failure rule: the copy stays, the fragment stays, and the
// condition turns False/IssueFailed until an issue succeeds again.
func TestRenewArtifactCredential_FailureKeepsCopyAndReportsIssueFailed(t *testing.T) {
	rec, client, issuer, clock := stsTestReconciler(t)
	cr := artifactCellController("cell-big-0", "big")
	pinnedProfile(client, cr)
	seedClientCRDs(client)

	rec.renewArtifactCredential(context.Background(), cr)
	first := string(client.createdSecrets["cell-big-0-artifacts"][artifactCredentialsKey])

	*clock = clock.Add(31 * time.Minute)
	issuer.err = errors.New("AccessDenied")
	rec.renewArtifactCredential(context.Background(), cr)
	if got := string(client.createdSecrets["cell-big-0-artifacts"][artifactCredentialsKey]); got != first {
		t.Error("a failed renewal must keep the current copy")
	}
	fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
	if fragment == "" {
		t.Error("a failed renewal must keep the fragment, so Jenkins keeps its artifact manager")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != ReasonArtifactIssueFailed || !strings.Contains(cond.Message, "AccessDenied") {
		t.Errorf("condition = %+v, want False/IssueFailed", cond)
	}
	if reason := cellConfigGateReason("1", rec.blobstoreURL, &cond); reason != v1alpha1.ReasonArtifactStorageLocal {
		t.Errorf("cell gate = %q, want ArtifactStorageLocal so placements stop", reason)
	}

	issuer.err = nil
	rec.renewArtifactCredential(context.Background(), cr)
	if len(issuer.requests) != 2 {
		t.Fatalf("issue calls = %d, want 2: a retry inside the interval must not call STS", len(issuer.requests))
	}
	if _, cond, _ = rec.artifactStorageFragment(context.Background(), cr); cond.Reason != ReasonArtifactIssueFailed {
		t.Errorf("condition inside the retry interval = %+v, want IssueFailed", cond)
	}

	*clock = clock.Add(artifactIssueRetryInterval)
	rec.renewArtifactCredential(context.Background(), cr)
	if got := string(client.createdSecrets["cell-big-0-artifacts"][artifactCredentialsKey]); got == first {
		t.Error("a successful renewal must rewrite the copy")
	}
	if _, cond, _ = rec.artifactStorageFragment(context.Background(), cr); cond.Reason != "External" {
		t.Errorf("condition after recovery = %+v, want External", cond)
	}
}

func TestRenewArtifactCredential_StaticModeAndNonCellDoNothing(t *testing.T) {
	rec, client, issuer, _ := stsTestReconciler(t)
	rec.renewArtifactCredential(context.Background(), testController("ci", "ns", v1alpha1.ControllerPhaseConnected))
	rec.SetArtifactSTS(ArtifactSTSConfig{})
	rec.renewArtifactCredential(context.Background(), artifactCellController("cell-big-0", "big"))
	if len(issuer.requests) != 0 || len(client.createdSecrets) != 0 {
		t.Errorf("issue calls = %d, writes = %d, want none", len(issuer.requests), len(client.createdSecrets))
	}
}

func TestReconcileArtifactSecretRoll_STSMode(t *testing.T) {
	cases := []struct {
		name      string
		cell      bool
		javaOpts  string
		found     bool
		noIssuer  bool
		issuerErr bool
		issueErr  bool
		wantRoll  bool
	}{
		{name: "pod predates STS", cell: true, javaOpts: "-Dx=1", found: true, wantRoll: true},
		{name: "pod reads the file", cell: true, javaOpts: "-Dx=1 " + artifactSharedCredentialsProperty, found: true},
		{name: "no StatefulSet", cell: true},
		{name: "non-cell", javaOpts: "-Dx=1", found: true},
		{name: "issuer missing", cell: true, javaOpts: "-Dx=1", found: true, noIssuer: true},
		{name: "issuer read fault", cell: true, javaOpts: "-Dx=1", found: true, issuerErr: true},
		{name: "issue failing", cell: true, javaOpts: "-Dx=1", found: true, issueErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, client, issuer, _ := stsTestReconciler(t)
			if tc.noIssuer {
				delete(client.existingSecrets, "varroa-artifact-issuer")
			}
			if tc.issuerErr {
				client.getSecretErrs = map[string]error{"varroa-artifact-issuer": errors.New("etcd unavailable")}
			}
			if tc.issueErr {
				issuer.err = errors.New("NotImplemented")
			}
			if tc.found {
				client.stsContainerEnv = map[string]map[string]string{overlay.JenkinsContainerName: {"JAVA_OPTS": tc.javaOpts}}
			}
			cr := testController("ci", "ns", v1alpha1.ControllerPhaseConnected)
			if tc.cell {
				cr = artifactCellController("cell-big-0", "big")
				cr.Status.Phase = v1alpha1.ControllerPhaseConnected
			}
			// The tick renews first, then runs the roll check.
			rec.renewArtifactCredential(context.Background(), cr)
			issued := len(issuer.requests)
			if got := rec.reconcileArtifactSecretRoll(context.Background(), cr); got != tc.wantRoll {
				t.Errorf("rolled = %v, want %v", got, tc.wantRoll)
			}
			if len(issuer.requests) != issued {
				t.Error("the roll check must never issue a credential")
			}
		})
	}
}

// TestEnsureArtifactSTSCredential_StaleIssueBacksOff covers a store whose
// clock trails the operator's: the issued credential is stale at once, so
// the operator records a failure and waits the retry interval.
func TestEnsureArtifactSTSCredential_StaleIssueBacksOff(t *testing.T) {
	rec, _, issuer, _ := stsTestReconciler(t)
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	issuer.now = func() time.Time { return clock.Add(-2 * time.Hour) }
	cr := artifactCellController("cell-big-0", "big")

	rec.renewArtifactCredential(context.Background(), cr)
	rec.renewArtifactCredential(context.Background(), cr)
	if len(issuer.requests) != 1 {
		t.Fatalf("issue calls = %d, want 1 inside the retry interval", len(issuer.requests))
	}
	if msg, failed := rec.artifactFailures.get(cr); !failed || !strings.Contains(msg, "before half of its lifetime") {
		t.Errorf("failure = %q, %v, want the stale-issue failure", msg, failed)
	}
}

func TestBuildStatefulSet_ArtifactSharedCredentialsProperty(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name: "cell-00000000", Namespace: "ns", ControllerName: "cell",
		ArtifactSecretName:        "cell-artifacts",
		ArtifactSharedCredentials: true,
	})
	env := builtContainerEnv(t, sts, "jenkins")
	if !strings.Contains(env["JAVA_OPTS"], "-Daws.sharedCredentialsFile=/run/secrets/varroa-artifacts/credentials") {
		t.Errorf("JAVA_OPTS = %q, want the shared credentials property", env["JAVA_OPTS"])
	}
	for k := range env {
		if strings.HasPrefix(k, "AWS_") {
			t.Errorf("env %s must not be set: a build process would inherit it", k)
		}
	}

	static := builtContainerEnv(t, buildStatefulSet(StatefulSetSpec{
		Name: "cell-00000000", Namespace: "ns", ControllerName: "cell", ArtifactSecretName: "cell-artifacts",
	}), "jenkins")
	if strings.Contains(static["JAVA_OPTS"], "aws.sharedCredentialsFile") {
		t.Error("static mode must not set the shared credentials property")
	}
}

// TestProvisioning_STSCellBuildsPodWithProperty drives a full provisioning
// tick for a pool cell in STS mode.
func TestProvisioning_STSCellBuildsPodWithProperty(t *testing.T) {
	rec, client, issuer, _ := stsTestReconciler(t)
	cr := artifactCellController("cell-big-0", "big")
	pinnedProfile(client, cr)
	seedClientCRDs(client)

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}
	if len(client.stsSpecs) != 1 {
		t.Fatalf("StatefulSets = %d, want 1", len(client.stsSpecs))
	}
	spec := client.stsSpecs[0]
	if spec.ArtifactSecretName != "cell-big-0-artifacts" || !spec.ArtifactSharedCredentials || spec.ArtifactSecretHash != "" {
		t.Errorf("spec = name %q shared %v hash %q", spec.ArtifactSecretName, spec.ArtifactSharedCredentials, spec.ArtifactSecretHash)
	}
	if len(issuer.requests) != 1 {
		t.Errorf("issue calls = %d, want 1", len(issuer.requests))
	}
}

func TestHandleProvisioning_STSIssueFailureBlocks(t *testing.T) {
	rec, client, issuer, _ := stsTestReconciler(t)
	issuer.err = errors.New("NotImplemented")
	cr := artifactCellController("cell-big-0", "big")
	pinnedProfile(client, cr)
	seedClientCRDs(client)

	if err := rec.handleProvisioning(context.Background(), cr); err == nil {
		t.Fatal("expected handleProvisioning to fail on an issue fault")
	}
	if len(client.stsSpecs) != 0 {
		t.Errorf("expected no StatefulSet, got %d", len(client.stsSpecs))
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if cond == nil || cond.Reason != v1alpha1.ReasonReconcileBlockedArtifactSecretFailed {
		t.Fatalf("ReconcileBlocked = %+v, want ArtifactSecretFailed", cond)
	}
}

// TestRenewArtifactCredential_FreshCopyReadsNoIssuer covers the cheap path:
// a fresh copy costs one read of the copy, no issuer read, and clears a
// failure that an earlier issuer fault left behind.
func TestRenewArtifactCredential_FreshCopyReadsNoIssuer(t *testing.T) {
	rec, client, issuer, _ := stsTestReconciler(t)
	cr := artifactCellController("cell-big-0", "big")
	rec.renewArtifactCredential(context.Background(), cr)
	rec.artifactFailures.record(cr, errors.New("issuer read fault"), time.Now())

	before := client.getSecretCalls["varroa-artifact-issuer"]
	rec.renewArtifactCredential(context.Background(), cr)
	if got := client.getSecretCalls["varroa-artifact-issuer"]; got != before {
		t.Errorf("issuer reads = %d, want %d", got, before)
	}
	if len(issuer.requests) != 1 {
		t.Errorf("issue calls = %d, want 1", len(issuer.requests))
	}
	if _, failed := rec.artifactFailures.get(cr); failed {
		t.Error("a fresh copy must clear the recorded failure")
	}
}
