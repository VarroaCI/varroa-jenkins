package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// TestParseBlobstoreURL covers task 5.1's table: an AWS URL, an https custom
// endpoint, an http custom endpoint with a port, a pgblob: URL, and an
// unparseable URL. The last two yield no fragment (IsS3 false, or ok
// false).
func TestParseBlobstoreURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want blobstoreParams
		ok   bool
	}{
		{
			name: "aws",
			url:  "s3://my-bucket?region=eu-west-1",
			want: blobstoreParams{IsS3: true, Container: "my-bucket", Region: "eu-west-1"},
			ok:   true,
		},
		{
			name: "https custom endpoint",
			url:  "s3://my-bucket?region=us-east-1&endpoint=https://s3.example.net&s3ForcePathStyle=true",
			want: blobstoreParams{IsS3: true, Container: "my-bucket", Region: "us-east-1", CustomEndpoint: "s3.example.net", UseHTTP: false, UsePathStyleURL: true},
			ok:   true,
		},
		{
			name: "http custom endpoint with port",
			url:  "s3://my-bucket?region=us-east-1&endpoint=http://minio.dev:9000&s3ForcePathStyle=true",
			want: blobstoreParams{IsS3: true, Container: "my-bucket", Region: "us-east-1", CustomEndpoint: "minio.dev:9000", UseHTTP: true, UsePathStyleURL: true},
			ok:   true,
		},
		{
			name: "pgblob yields no fragment",
			url:  "pgblob:",
			want: blobstoreParams{},
			ok:   true,
		},
		{
			name: "unparseable yields no fragment",
			url:  "s3://bad host/path",
			want: blobstoreParams{},
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseBlobstoreURL(tc.url)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("parseBlobstoreURL(%q) = %+v, want %+v", tc.url, got, tc.want)
			}
			if tc.name == "pgblob yields no fragment" && got.IsS3 {
				t.Error("pgblob: must yield IsS3=false")
			}
		})
	}
}

// TestArtifactManagerPrefix asserts the rendered prefix names the three
// identity tokens and ends with a slash (task 5.3b).
func TestArtifactManagerPrefix(t *testing.T) {
	got := artifactManagerPrefix("core", "team-a", "web")
	want := "artifacts/core/team-a/web/"
	if got != want {
		t.Errorf("artifactManagerPrefix = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, "/") {
		t.Error("prefix must end with a slash")
	}
}

// TestRenderArtifactStorageFragment asserts every key task 0.6's constant
// list names is present at its exact path, using a YAML round trip rather
// than string matching.
func TestRenderArtifactStorageFragment(t *testing.T) {
	p := blobstoreParams{IsS3: true, Container: "spike-bucket", Region: "us-east-1", CustomEndpoint: "minio.example:9000", UseHTTP: true, UsePathStyleURL: true}
	out, err := renderArtifactStorageFragment(p, "core", "varroa", "cell-a", false)
	if err != nil {
		t.Fatalf("renderArtifactStorageFragment: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal fragment: %v", err)
	}

	get := func(path ...string) any {
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

	if got := get("aws", "awsCredentials", "credentialsId"); got != artifactCredentialID {
		t.Errorf("aws.awsCredentials.credentialsId = %v, want %v", got, artifactCredentialID)
	}
	if got := get("aws", "awsCredentials", "region"); got != "us-east-1" {
		t.Errorf("aws.awsCredentials.region = %v", got)
	}
	if got := get("aws", "s3", "container"); got != "spike-bucket" {
		t.Errorf("aws.s3.container = %v", got)
	}
	if got := get("aws", "s3", "prefix"); got != "artifacts/core/varroa/cell-a/" {
		t.Errorf("aws.s3.prefix = %v", got)
	}
	if got := get("aws", "s3", "customEndpoint"); got != "minio.example:9000" {
		t.Errorf("aws.s3.customEndpoint = %v", got)
	}
	if got := get("aws", "s3", "usePathStyleUrl"); got != true {
		t.Errorf("aws.s3.usePathStyleUrl = %v", got)
	}
	if got := get("aws", "s3", "useHttp"); got != true {
		t.Errorf("aws.s3.useHttp = %v", got)
	}
	if got := get("aws", "s3", "disableSessionToken"); got != true {
		t.Errorf("aws.s3.disableSessionToken = %v, want true (load-bearing per D6)", got)
	}
	factories, _ := get("unclassified", "artifactManager", "artifactManagerFactories").([]any)
	if len(factories) != 1 {
		t.Fatalf("artifactManagerFactories = %v, want 1 entry", factories)
	}
	entry, _ := factories[0].(map[string]any)
	jclouds, _ := entry["jclouds"].(map[string]any)
	if jclouds["provider"] != "s3" {
		t.Errorf("jclouds.provider = %v, want s3", jclouds["provider"])
	}

	// No secret value ever appears in the fragment.
	if strings.Contains(out, "accessKey") || strings.Contains(out, "secretKey") {
		t.Error("fragment must carry no credential material; the credential is injected separately")
	}
}

// TestRenderArtifactStorageFragment_NoCustomEndpoint asserts an AWS-default
// URL omits customEndpoint rather than writing an empty string.
func TestRenderArtifactStorageFragment_NoCustomEndpoint(t *testing.T) {
	p := blobstoreParams{IsS3: true, Container: "b", Region: "eu-west-1"}
	out, err := renderArtifactStorageFragment(p, "core", "ns", "cell", false)
	if err != nil {
		t.Fatalf("renderArtifactStorageFragment: %v", err)
	}
	if strings.Contains(out, "customEndpoint") {
		t.Errorf("expected no customEndpoint key for an AWS-default URL, got:\n%s", out)
	}
}

// TestStripArtifactManager strips a bundle-authored key and a
// profile-overlay-authored key from a merged document (task 5.2).
func TestStripArtifactManager(t *testing.T) {
	merged := `
jenkins:
  systemMessage: "hello"
unclassified:
  artifactManager:
    artifactManagerFactories:
      - jclouds:
          provider: file
  someOtherPlugin:
    setting: keep-me
aws:
  awsCredentials:
    credentialsId: bundle-cred
    region: us-west-2
  s3:
    container: bundle-bucket
    prefix: bundle-prefix/
`
	out := stripArtifactManager(merged)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal stripped doc: %v", err)
	}
	if _, ok := doc["aws"]; ok {
		t.Errorf("expected aws root fully stripped, got %v", doc["aws"])
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	if unclassified == nil {
		t.Fatal("unclassified root must survive (someOtherPlugin key)")
	}
	if _, ok := unclassified["artifactManager"]; ok {
		t.Errorf("expected unclassified.artifactManager stripped, got %v", unclassified["artifactManager"])
	}
	other, _ := unclassified["someOtherPlugin"].(map[string]any)
	if other == nil || other["setting"] != "keep-me" {
		t.Errorf("expected unrelated unclassified.someOtherPlugin key preserved, got %v", unclassified["someOtherPlugin"])
	}
	jenkins, _ := doc["jenkins"].(map[string]any)
	if jenkins == nil || jenkins["systemMessage"] != "hello" {
		t.Errorf("expected unrelated jenkins key preserved, got %v", doc["jenkins"])
	}
}

// TestStripArtifactManager_NoOp asserts a document with none of the three
// subtrees returns unchanged.
func TestStripArtifactManager_NoOp(t *testing.T) {
	doc := "jenkins:\n  systemMessage: hi\n"
	if got := stripArtifactManager(doc); got != doc {
		t.Errorf("expected no-op strip, got %q", got)
	}
}

// TestInjectArtifactCredential asserts two bundle-authored credentials
// survive alongside the injected one, and a second run still yields exactly
// three (task 5.2a).
func TestInjectArtifactCredential(t *testing.T) {
	base := `
credentials:
  system:
    domainCredentials:
      - credentials:
          - usernamePassword:
              id: bundle-cred-1
              username: alice
      - domain:
          name: github.com
        credentials:
          - usernamePassword:
              id: bundle-cred-2
              username: bob
`
	out, err := injectArtifactCredential(base)
	if err != nil {
		t.Fatalf("injectArtifactCredential: %v", err)
	}
	ids := credentialIDs(t, out)
	if len(ids) != 3 {
		t.Fatalf("expected 3 credentials after injection, got %d: %v", len(ids), ids)
	}
	assertContains(t, ids, "bundle-cred-1", "bundle-cred-2", artifactCredentialID)

	// Idempotent: running again on the already-injected document still
	// yields exactly three, not four.
	out2, err := injectArtifactCredential(out)
	if err != nil {
		t.Fatalf("injectArtifactCredential (second run): %v", err)
	}
	ids2 := credentialIDs(t, out2)
	if len(ids2) != 3 {
		t.Fatalf("expected 3 credentials after a second run, got %d: %v", len(ids2), ids2)
	}

	if !strings.Contains(out, "${readFile:"+artifactSecretMountPath+"/access-key-id}") {
		t.Error("expected the injected credential to carry a readFile reference, not a value")
	}
}

func credentialIDs(t *testing.T, yamlDoc string) []string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(yamlDoc), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	credentials, _ := doc["credentials"].(map[string]any)
	system, _ := credentials["system"].(map[string]any)
	domainCredentials, _ := system["domainCredentials"].([]any)
	var ids []string
	for _, d := range domainCredentials {
		dm, _ := d.(map[string]any)
		creds, _ := dm["credentials"].([]any)
		for _, c := range creds {
			cm, _ := c.(map[string]any)
			for _, v := range cm {
				vm, ok := v.(map[string]any)
				if !ok {
					continue
				}
				if id, ok := vm["id"].(string); ok {
					ids = append(ids, id)
				}
			}
		}
	}
	return ids
}

func assertContains(t *testing.T, haystack []string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		found := false
		for _, h := range haystack {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in %v", w, haystack)
		}
	}
}

// TestInjectArtifactCredential_GlobalDomainCreatedWhenAbsent asserts the
// global domain (no "domain" key) is created when the document has none.
func TestInjectArtifactCredential_GlobalDomainCreatedWhenAbsent(t *testing.T) {
	out, err := injectArtifactCredential("")
	if err != nil {
		t.Fatalf("injectArtifactCredential: %v", err)
	}
	ids := credentialIDs(t, out)
	assertContains(t, ids, artifactCredentialID)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	credentials, _ := doc["credentials"].(map[string]any)
	system, _ := credentials["system"].(map[string]any)
	domainCredentials, _ := system["domainCredentials"].([]any)
	if len(domainCredentials) != 1 {
		t.Fatalf("expected exactly one domain (the global one), got %d", len(domainCredentials))
	}
	dm, _ := domainCredentials[0].(map[string]any)
	if _, hasDomain := dm["domain"]; hasDomain {
		t.Error("the created domain must be the global one (no \"domain\" key)")
	}
}

// artifactTestReconciler builds a Reconciler wired for artifact-storage
// tests: cluster "core", and (when withSecret) a per-cell artifact Secret
// "abase-test" holding the two required keys.
func artifactTestReconciler(t *testing.T, withSecret bool) (*Reconciler, *testClient) {
	t.Helper()
	client := newTestClientWithBundle()
	client.existingSecrets = map[string]map[string][]byte{}
	if withSecret {
		client.existingSecrets["abase-test"] = map[string][]byte{
			"access-key-id":     []byte("AKIAEXAMPLE"),
			"secret-access-key": []byte("secretvalue"),
		}
	}
	rec := newTestReconciler(client)
	rec.SetClusterName("core")
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")
	return rec, client
}

// pinnedProfile seeds a JenkinsVersionProfile matching cr.Spec.Version whose
// resolved plugin set pins artifact-manager-s3.
func pinnedProfile(client *testClient, cr *v1alpha1.Controller) {
	cr.Spec.Version = "2.570"
	client.profiles["jenkins-2-570"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-2-570"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.570"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "profile-content-artifacts",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type:   "PluginSetReady",
				Status: metav1.ConditionTrue,
			}},
		},
	}
	client.configMapData["profile-content-artifacts"] = map[string]string{
		"plugins.yaml": "plugins:\n  - artifactId: artifact-manager-s3\n    version: \"986.v\"\n",
	}
}

// unpinnedProfile seeds a JenkinsVersionProfile matching cr.Spec.Version
// whose resolved plugin set carries plugins but does not include
// artifact-manager-s3 (task 7.3). Unlike the "no profile seeded" case, this
// does not depend on the embedded pluginlock baseline also lacking the
// plugin, so it stays a valid PluginMissing proof once task 7.2 (not this
// batch's task) pins artifact-manager-s3 into that baseline.
func unpinnedProfile(client *testClient, cr *v1alpha1.Controller) {
	cr.Spec.Version = "2.570"
	client.profiles["jenkins-2-570"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-2-570"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.570"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "profile-content-no-artifacts",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type:   "PluginSetReady",
				Status: metav1.ConditionTrue,
			}},
		},
	}
	client.configMapData["profile-content-no-artifacts"] = map[string]string{
		"plugins.yaml": "plugins:\n  - artifactId: git\n    version: \"5.0\"\n",
	}
}

// TestArtifactStorageFragment_EmitAndSilentCases covers task 5.3's emitting
// case and both silent cases (blob mode not S3, plugin not pinned).
func TestArtifactStorageFragment_EmitAndSilentCases(t *testing.T) {
	t.Run("emits when all conditions hold", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		pinnedProfile(client, cr)
		seedClientCRDs(client)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment == "" {
			t.Fatal("expected a rendered fragment")
		}
		if cond.Type != ConditionArtifactStorage || cond.Status != metav1.ConditionTrue || cond.Reason != "External" {
			t.Errorf("condition = %+v, want True/External", cond)
		}
	})

	t.Run("silent: blob mode is not s3", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		rec.SetBlobstoreURL("pgblob:")
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment != "" {
			t.Errorf("expected no fragment for postgres blob mode, got %q", fragment)
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != "LocalOnly" {
			t.Errorf("condition = %+v, want False/LocalOnly", cond)
		}
	})

	t.Run("silent: plugin not pinned", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		seedProfileWithoutArtifactManager(client)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.Version = "2.570"

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment != "" {
			t.Errorf("expected no fragment when the plugin isn't pinned, got %q", fragment)
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != "PluginMissing" {
			t.Errorf("condition = %+v, want False/PluginMissing", cond)
		}
	})

	t.Run("silent: profile pins a set without artifact-manager-s3", func(t *testing.T) {
		// Task 7.3: coreSetForCr resolves a seeded JenkinsVersionProfile
		// (exact-version match), and its resolved set reaches the same
		// PluginMissing branch as the no-profile fallback above.
		rec, client := artifactTestReconciler(t, true)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		unpinnedProfile(client, cr)
		seedClientCRDs(client)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment != "" {
			t.Errorf("expected no fragment when the resolved profile doesn't pin the plugin, got %q", fragment)
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != "PluginMissing" {
			t.Errorf("condition = %+v, want False/PluginMissing", cond)
		}
	})

	t.Run("silent: per-cell secret absent", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment != "" {
			t.Errorf("expected no fragment when the per-cell secret is absent, got %q", fragment)
		}
		if cond.Status != metav1.ConditionFalse || cond.Reason != "LocalOnly" {
			t.Errorf("condition = %+v, want False/LocalOnly", cond)
		}
		if !strings.Contains(cond.Message, "abase-test") {
			t.Errorf("condition message must name the missing secret, got %q", cond.Message)
		}
	})
}

// TestApplyArtifactStorage_MergeOrder asserts the whole merge order from
// the version-jcasc-overlay delta: no-fragment leaves the input untouched
// (no strip either), and the emitting case strips, merges, and injects, in
// that order (task 5.3a).
func TestApplyArtifactStorage_MergeOrder(t *testing.T) {
	base := "jenkins:\n  systemMessage: hi\naws:\n  s3:\n    container: bundle-bucket\n"

	t.Run("no fragment leaves the overlay result untouched", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false) // no secret -> no fragment
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

		got, cond, err := rec.applyArtifactStorage(context.Background(), cr, base)
		if err != nil {
			t.Fatalf("applyArtifactStorage: %v", err)
		}
		if got != base {
			t.Errorf("expected untouched passthrough, got:\n%s\nwant:\n%s", got, base)
		}
		if cond.Reason != "LocalOnly" {
			t.Errorf("cond.Reason = %q, want LocalOnly", cond.Reason)
		}
	})

	t.Run("fragment applies: strip, merge, inject", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		pinnedProfile(client, cr)
		seedClientCRDs(client)

		got, cond, err := rec.applyArtifactStorage(context.Background(), cr, base)
		if err != nil {
			t.Fatalf("applyArtifactStorage: %v", err)
		}
		if cond.Reason != "External" {
			t.Fatalf("cond.Reason = %q, want External", cond.Reason)
		}
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
			t.Fatalf("unmarshal merged doc: %v", err)
		}
		jenkins, _ := doc["jenkins"].(map[string]any)
		if jenkins == nil || jenkins["systemMessage"] != "hi" {
			t.Errorf("expected the base jenkins key preserved, got %v", doc["jenkins"])
		}
		aws, _ := doc["aws"].(map[string]any)
		s3, _ := aws["s3"].(map[string]any)
		if s3["container"] != "my-bucket" {
			t.Errorf("expected the bundle-authored aws.s3.container stripped and replaced, got %v", s3["container"])
		}
		ids := credentialIDs(t, got)
		assertContains(t, ids, artifactCredentialID)
	})
}

// TestResolveArtifactSecret covers tasks 5.1a/5.1b: the per-cell hit and
// projection, the owner reference, the absent case, and a rotation that
// changes the hash.
func TestResolveArtifactSecret(t *testing.T) {
	t.Run("absent source secret", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

		name, hash, err := rec.resolveArtifactSecret(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolveArtifactSecret: %v", err)
		}
		if name != "" || hash != "" {
			t.Fatalf("expected an empty name and hash, got name=%q hash=%q", name, hash)
		}
	})

	t.Run("first projection carries the owner reference", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

		name, hash, err := rec.resolveArtifactSecret(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolveArtifactSecret: %v", err)
		}
		if name != "test-artifacts" {
			t.Errorf("projected name = %q, want test-artifacts", name)
		}
		if hash == "" {
			t.Error("expected a non-empty data hash")
		}
		data := client.createdSecrets[name]
		if string(data["access-key-id"]) != "AKIAEXAMPLE" {
			t.Errorf("projected data = %v, want the source data verbatim", data)
		}
		owner := client.secretOwners[name]
		if owner.Kind != "Controller" || owner.Name != cr.Name || owner.UID != cr.UID {
			t.Errorf("owner reference = %+v, want a Controller ref to %s/%s", owner, cr.Namespace, cr.Name)
		}
		if owner.Controller == nil || !*owner.Controller {
			t.Error("expected Controller=true on the owner reference")
		}
	})

	t.Run("rotation changes the hash", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		seedClientCRDs(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

		_, hash1, err := rec.resolveArtifactSecret(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolveArtifactSecret: %v", err)
		}
		client.existingSecrets["abase-test"] = map[string][]byte{
			"access-key-id":     []byte("AKIAROTATED"),
			"secret-access-key": []byte("newsecret"),
		}
		_, hash2, err := rec.resolveArtifactSecret(context.Background(), cr)
		if err != nil {
			t.Fatalf("resolveArtifactSecret (after rotation): %v", err)
		}
		if hash1 == hash2 {
			t.Error("expected the hash to change after a rotation")
		}
	})
}

// TestArtifactStorageConditionReasons drives all three ArtifactStorage
// reasons through PatchControllerStatus via a full provisioning reconcile
// (task 5.6).
func TestArtifactStorageConditionReasons(t *testing.T) {
	t.Run("LocalOnly: blobstore url unset", func(t *testing.T) {
		client := newTestClientWithBundle()
		rec := newTestReconciler(client)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		_ = rec.reconcileController(context.Background(), cr)
		cond := findCondition(cr.Status.Conditions, ConditionArtifactStorage)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "LocalOnly" {
			t.Fatalf("condition = %+v, want False/LocalOnly", cond)
		}
	})

	t.Run("PluginMissing: profile does not pin the plugin", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.existingSecrets = map[string]map[string][]byte{
			"abase-test": {"access-key-id": []byte("a"), "secret-access-key": []byte("b")},
		}
		seedProfileWithoutArtifactManager(client)
		rec := newTestReconciler(client)
		rec.SetArtifactSecretBase("abase")
		rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		cr.Spec.Version = "2.570"
		_ = rec.reconcileController(context.Background(), cr)
		cond := findCondition(cr.Status.Conditions, ConditionArtifactStorage)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "PluginMissing" {
			t.Fatalf("condition = %+v, want False/PluginMissing", cond)
		}
	})

	t.Run("External: fragment applies", func(t *testing.T) {
		client := newTestClientWithBundle()
		client.existingSecrets = map[string]map[string][]byte{
			"abase-test": {"access-key-id": []byte("a"), "secret-access-key": []byte("b")},
		}
		rec := newTestReconciler(client)
		rec.SetArtifactSecretBase("abase")
		rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		pinnedProfile(client, cr)
		seedClientCRDs(client)
		if err := rec.reconcileController(context.Background(), cr); err != nil {
			t.Fatalf("reconcileController: %v", err)
		}
		cond := findCondition(cr.Status.Conditions, ConditionArtifactStorage)
		if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "External" {
			t.Fatalf("condition = %+v, want True/External", cond)
		}
		// The projected artifact Secret and the StatefulSet mount both exist.
		if len(client.stsSpecs) == 0 {
			t.Fatal("expected a StatefulSet to be built")
		}
		last := client.stsSpecs[len(client.stsSpecs)-1]
		if last.ArtifactSecretName == "" {
			t.Error("expected StatefulSetSpec.ArtifactSecretName to be set")
		}
	})
}

// TestCreateStatefulSet_ArtifactMount asserts the artifact-secret volume and
// mount, and that the rendered ConfigMap holds no key material (task 5.4) —
// the fragment carries only a readFile reference.
func TestCreateStatefulSet_ArtifactMount(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
		ArtifactSecretName: "ci-artifacts",
		ArtifactSecretHash: "deadbeef",
	})

	vols := builtVolumes(t, sts)
	found := false
	for _, v := range vols {
		if v["name"] == "varroa-artifacts" {
			found = true
			secret, _ := v["secret"].(map[string]interface{})
			if secret["secretName"] != "ci-artifacts" {
				t.Errorf("volume secretName = %v, want ci-artifacts", secret["secretName"])
			}
		}
	}
	if !found {
		t.Fatal("expected a varroa-artifacts volume")
	}

	mounts := builtContainerVolumeMounts(t, sts, "jenkins")
	mountFound := false
	for _, m := range mounts {
		if m["name"] == "varroa-artifacts" {
			mountFound = true
			if m["mountPath"] != artifactSecretMountPath {
				t.Errorf("mountPath = %v, want %v", m["mountPath"], artifactSecretMountPath)
			}
			if m["readOnly"] != true {
				t.Error("expected the artifact secret mount to be readOnly")
			}
		}
	}
	if !mountFound {
		t.Fatal("expected the jenkins container to mount varroa-artifacts")
	}
}

// TestCreateStatefulSet_ArtifactSecretHashAnnotation asserts the
// pod-template annotation CreateStatefulSet stamps from
// ArtifactSecretHash, mirroring the CASC content hash mechanism (task
// 5.1a).
func TestCreateStatefulSet_ArtifactSecretHashAnnotation(t *testing.T) {
	c, dyn := newFakeStsClient()
	spec := baseCascHashSpec("ci-artifacts-hash", "")
	spec.ArtifactSecretName = "ci-artifacts"
	spec.ArtifactSecretHash = "deadbeef"

	if err := c.CreateStatefulSet(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := dyn.Resource(stsGVR).Namespace("ns").Get(context.Background(), "ci-artifacts-hash", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	v, ok := podTemplateAnnotation(t, got, artifactSecretHashAnnotation)
	if !ok || v != "deadbeef" {
		t.Fatalf("annotation %s = %q, ok=%v, want deadbeef", artifactSecretHashAnnotation, v, ok)
	}
}

// TestCreateStatefulSet_NoArtifactSecret asserts no volume/mount/annotation
// appears when ArtifactSecretName is empty.
func TestCreateStatefulSet_NoArtifactSecret(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci"})
	for _, v := range builtVolumes(t, sts) {
		if v["name"] == "varroa-artifacts" {
			t.Fatal("expected no varroa-artifacts volume when ArtifactSecretName is unset")
		}
	}
	for _, m := range builtContainerVolumeMounts(t, sts, "jenkins") {
		if m["name"] == "varroa-artifacts" {
			t.Fatal("expected no varroa-artifacts mount when ArtifactSecretName is unset")
		}
	}
}

// TestCreateStatefulSet_MiteLogURLEnv covers both branches of task 5.5:
// VARROA_MITE_LOG_URL set only when the record store is enabled.
func TestCreateStatefulSet_MiteLogURLEnv(t *testing.T) {
	cases := []struct {
		name      string
		claimGate string
		wantSet   bool
	}{
		{name: "record store enabled", claimGate: "1", wantSet: true},
		{name: "record store disabled", claimGate: "0", wantSet: false},
		{name: "unset", claimGate: "", wantSet: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := buildStatefulSet(StatefulSetSpec{
				Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
				ClaimGate: tc.claimGate,
			})
			env := builtContainerEnv(t, sts, "jenkins")
			got, set := env["VARROA_MITE_LOG_URL"]
			if set != tc.wantSet {
				t.Fatalf("VARROA_MITE_LOG_URL set = %v, want %v", set, tc.wantSet)
			}
			if tc.wantSet && got != "http://127.0.0.1:9095/v1/log" {
				t.Errorf("VARROA_MITE_LOG_URL = %q", got)
			}
		})
	}
}

// TestCreateStatefulSet_MiteRunStateURLEnv covers both branches of the
// pipeline mesh's own gate. RunStatePusher reads
// VARROA_MITE_RUN_STATE_URL and pushes nothing without it, so the
// variable rides the record-store gate the log URL already rides.
func TestCreateStatefulSet_MiteRunStateURLEnv(t *testing.T) {
	cases := []struct {
		name      string
		claimGate string
		wantSet   bool
	}{
		{name: "record store enabled", claimGate: "1", wantSet: true},
		{name: "record store disabled", claimGate: "0", wantSet: false},
		{name: "unset", claimGate: "", wantSet: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sts := buildStatefulSet(StatefulSetSpec{
				Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
				ClaimGate: tc.claimGate,
			})
			env := builtContainerEnv(t, sts, "jenkins")
			got, set := env["VARROA_MITE_RUN_STATE_URL"]
			if set != tc.wantSet {
				t.Fatalf("VARROA_MITE_RUN_STATE_URL set = %v, want %v", set, tc.wantSet)
			}
			if tc.wantSet && got != "http://127.0.0.1:9095" {
				t.Errorf("VARROA_MITE_RUN_STATE_URL = %q", got)
			}
		})
	}
}

// TestCreateStatefulSet_MiteRunStateURLCarriesNoPath asserts the
// variable carries the loopback base and no route. RunStatePusher
// appends its own path to this value, so a value that already carries
// the path makes every push target that path twice and store nothing.
// The plugin's own constant is /v1/run-state, and the two compose to
// the route the mite serves.
func TestCreateStatefulSet_MiteRunStateURLCarriesNoPath(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
		ClaimGate: "1",
	})
	env := builtContainerEnv(t, sts, "jenkins")
	base := env["VARROA_MITE_RUN_STATE_URL"]
	if strings.Contains(strings.TrimPrefix(base, "http://"), "/") {
		t.Fatalf("VARROA_MITE_RUN_STATE_URL = %q, want a base with no path", base)
	}
	const pluginRoute = "/v1/run-state"
	if composed := base + pluginRoute; composed != "http://127.0.0.1:9095/v1/run-state" {
		t.Fatalf("the base and the plugin route compose to %q", composed)
	}
}

// TestCreateStatefulSet_MiteURLsShareOneGate asserts the two loopback
// URLs are stamped together. A pod that pushes its console log but no
// run state replicates nothing, and that loss stays silent until a cell
// dies.
func TestCreateStatefulSet_MiteURLsShareOneGate(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{
		Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci",
		ClaimGate: "1",
	})
	env := builtContainerEnv(t, sts, "jenkins")
	_, logSet := env["VARROA_MITE_LOG_URL"]
	_, stateSet := env["VARROA_MITE_RUN_STATE_URL"]
	if logSet != stateSet {
		t.Fatalf("log URL set = %v, run-state URL set = %v; both take one gate", logSet, stateSet)
	}
}

// TestCreateStatefulSet_CellInstanceEnv asserts VARROA_CELL_INSTANCE on both
// containers, sourced from the downward API field metadata.uid (task 5.5a).
func TestCreateStatefulSet_CellInstanceEnv(t *testing.T) {
	sts := buildStatefulSet(StatefulSetSpec{Name: "ci-00000000", Namespace: "team-a", ControllerName: "ci"})
	podSpec := sts.Object["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	containers := podSpec["containers"].([]map[string]interface{})

	for _, cn := range []string{"jenkins", "mite"} {
		var cm map[string]interface{}
		for _, c := range containers {
			if c["name"] == cn {
				cm = c
			}
		}
		if cm == nil {
			t.Fatalf("container %q not found", cn)
		}
		entries, _ := cm["env"].([]map[string]interface{})
		var entry map[string]interface{}
		for _, e := range entries {
			if e["name"] == "VARROA_CELL_INSTANCE" {
				entry = e
			}
		}
		if entry == nil {
			t.Fatalf("container %q missing VARROA_CELL_INSTANCE", cn)
		}
		valueFrom, _ := entry["valueFrom"].(map[string]interface{})
		fieldRef, _ := valueFrom["fieldRef"].(map[string]interface{})
		if fieldRef["fieldPath"] != "metadata.uid" {
			t.Errorf("container %q VARROA_CELL_INSTANCE fieldRef.fieldPath = %v, want metadata.uid", cn, fieldRef["fieldPath"])
		}
	}
}

// TestFindUnresolvedVars_ArtifactFragmentPassesThrough asserts the
// readFile reference the credential injector writes survives
// findUnresolvedVars unchanged (task 5.7).
func TestFindUnresolvedVars_ArtifactFragmentPassesThrough(t *testing.T) {
	p := blobstoreParams{IsS3: true, Container: "b", Region: "us-east-1"}
	fragment, err := renderArtifactStorageFragment(p, "core", "ns", "cell", false)
	if err != nil {
		t.Fatalf("renderArtifactStorageFragment: %v", err)
	}
	withCred, err := injectArtifactCredential(fragment)
	if err != nil {
		t.Fatalf("injectArtifactCredential: %v", err)
	}
	if got := findUnresolvedVars(true, withCred); len(got) != 0 {
		t.Errorf("findUnresolvedVars = %v, want empty (readFile refs must pass through)", got)
	}
}

// seedProfileWithoutArtifactManager pins a version profile whose plugin set
// lacks artifact-manager-s3. The embedded baseline pins that plugin, so a
// test of the PluginMissing reason needs a profile that does not.
func seedProfileWithoutArtifactManager(client *testClient) {
	profile := &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "jenkins-2-570"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.570"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "profile-content",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type:   "PluginSetReady",
				Status: metav1.ConditionTrue,
			}},
		},
	}
	client.profiles["jenkins-2-570"] = profile
	client.configMapData["profile-content"] = map[string]string{
		"plugins.yaml": "core:\n  - git\nplugins:\n  - artifactId: git\n    version: \"5.10.1\"\n",
	}
}

// TestReconcileArtifactSecretRoll covers the "A rotated credential reaches
// a running cell" scenario: a Connected controller whose live StatefulSet
// carries a stale artifact-secret hash transitions to Provisioning, and
// every other state is a no-op.
func TestReconcileArtifactSecretRoll(t *testing.T) {
	ctx := context.Background()
	sourceHash := secretDataHash(map[string][]byte{
		"access-key-id":     []byte("AKIAEXAMPLE"),
		"secret-access-key": []byte("secretvalue"),
	})
	t.Run("stale hash transitions to Provisioning", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		client.stsArtifactSecretHashFound = true
		client.stsArtifactSecretHash = "stale"
		cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
		if !rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected a transition")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
		pc := findCondition(cr.Status.Conditions, v1alpha1.ConditionProvisioning)
		if pc == nil || pc.Status != metav1.ConditionTrue || pc.Reason != "ProvisioningStarted" {
			t.Errorf("Provisioning condition = %+v, want True/ProvisioningStarted", pc)
		}
	})
	t.Run("a Secret added after provisioning transitions too", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		client.stsArtifactSecretHashFound = true
		client.stsArtifactSecretHash = ""
		cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
		if !rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected a transition when the StatefulSet mounts no Secret yet")
		}
	})
	t.Run("converged hash is a no-op", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		client.stsArtifactSecretHashFound = true
		client.stsArtifactSecretHash = sourceHash
		cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
		if rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected no transition when converged")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase = %q, want Connected", cr.Status.Phase)
		}
	})
	t.Run("absent source Secret is a no-op", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		client.stsArtifactSecretHashFound = true
		client.stsArtifactSecretHash = "anything"
		cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
		if rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected no transition without a source Secret")
		}
	})
	t.Run("missing StatefulSet is a no-op", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		client.stsArtifactSecretHashFound = false
		cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
		if rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected no transition without a StatefulSet")
		}
	})
	t.Run("read error is a no-op", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		client.stsArtifactSecretHashErr = errors.New("boom")
		cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
		if rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected no transition on a read error")
		}
	})
	t.Run("a lookup fault skips the tick without rolling", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, true)
		client.getSecretErrs = map[string]error{"abase-test": errors.New("etcd unavailable")}
		client.stsArtifactSecretHashFound = true
		client.stsArtifactSecretHash = "stale"
		cr := testController("test", "ns", v1alpha1.ControllerPhaseConnected)
		if rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected no transition on a lookup fault")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseConnected {
			t.Errorf("phase = %q, want unchanged Connected", cr.Status.Phase)
		}
	})
	t.Run("a rotated pool secret moves a connected labelled cell to Provisioning", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		client.existingSecrets["abase-big"] = map[string][]byte{
			"access-key-id":     []byte("AKIAEXAMPLE"),
			"secret-access-key": []byte("secretvalue"),
		}
		client.stsArtifactSecretHashFound = true
		client.stsArtifactSecretHash = "stale"
		cr := artifactCellController("cell-big-0", "big")
		cr.Status.Phase = v1alpha1.ControllerPhaseConnected
		if !rec.reconcileArtifactSecretRoll(ctx, cr) {
			t.Fatal("expected a transition")
		}
		if cr.Status.Phase != v1alpha1.ControllerPhaseProvisioning {
			t.Errorf("phase = %q, want Provisioning", cr.Status.Phase)
		}
	})
}

// artifactCellController returns a cell Controller named name in namespace
// "ns", carrying varroa.dev/cell-pool=pool when pool is non-empty.
func artifactCellController(name, pool string) *v1alpha1.Controller {
	cr := testController(name, "ns", v1alpha1.ControllerPhaseProvisioning)
	cr.Spec.Cell = &v1alpha1.CellSpec{Enabled: true}
	if pool != "" {
		cr.Labels = map[string]string{cells.LabelCellPool: pool}
	}
	return cr
}

// TestArtifactSecretCandidates_Fragment covers task 1.9: the candidate
// order, the per-cell/pool precedence, and the exhausted-list message.
func TestArtifactSecretCandidates_Fragment(t *testing.T) {
	t.Run("per-cell secret wins over the pool secret", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		client.existingSecrets["abase-cell-big-0"] = map[string][]byte{
			"access-key-id": []byte("a"), "secret-access-key": []byte("b"),
		}
		client.existingSecrets["abase-big"] = map[string][]byte{
			"access-key-id": []byte("c"), "secret-access-key": []byte("d"),
		}
		cr := artifactCellController("cell-big-0", "big")
		pinnedProfile(client, cr)
		seedClientCRDs(client)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment == "" {
			t.Fatal("expected a rendered fragment")
		}
		if cond.Status != metav1.ConditionTrue || cond.Reason != "External" {
			t.Fatalf("condition = %+v, want True/External", cond)
		}
		if !strings.Contains(cond.Message, "abase-cell-big-0") {
			t.Errorf("message must name the per-cell secret, got %q", cond.Message)
		}
	})

	t.Run("the pool secret serves a pool-minted cell", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		client.existingSecrets["abase-big"] = map[string][]byte{
			"access-key-id": []byte("c"), "secret-access-key": []byte("d"),
		}
		cr := artifactCellController("cell-big-2", "big")
		pinnedProfile(client, cr)
		seedClientCRDs(client)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment == "" {
			t.Fatal("expected a rendered fragment")
		}
		if cond.Status != metav1.ConditionTrue || cond.Reason != "External" {
			t.Fatalf("condition = %+v, want True/External", cond)
		}
		if !strings.Contains(cond.Message, "abase-big") {
			t.Errorf("message must name the pool secret, got %q", cond.Message)
		}
	})

	t.Run("a deleted pool keeps the cell's credential", func(t *testing.T) {
		// No CellPool object is seeded here: the pool name comes from the
		// label string alone (design D2), not from a live CellPool.
		rec, client := artifactTestReconciler(t, false)
		client.existingSecrets["abase-big"] = map[string][]byte{
			"access-key-id": []byte("c"), "secret-access-key": []byte("d"),
		}
		cr := artifactCellController("cell-big-2", "big")
		pinnedProfile(client, cr)
		seedClientCRDs(client)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment == "" || cond.Reason != "External" {
			t.Fatalf("expected the pool secret to still resolve, cond=%+v", cond)
		}
	})

	t.Run("no candidate lists both names in order", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		cr := artifactCellController("cell-big-2", "big")
		seedClientCRDs(client)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment != "" {
			t.Errorf("expected no fragment, got %q", fragment)
		}
		want := `artifact secret not found: tried "abase-cell-big-2", "abase-big"`
		if cond.Message != want {
			t.Errorf("message = %q, want %q", cond.Message, want)
		}
	})

	t.Run("an unlabelled cell tries only its own name", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		cr := artifactCellController("cell-lonely", "")
		seedClientCRDs(client)

		_, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		want := `artifact secret not found: tried "abase-cell-lonely"`
		if cond.Message != want {
			t.Errorf("message = %q, want %q", cond.Message, want)
		}
	})

	t.Run("a non-cell controller tries only its own name", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		seedClientCRDs(client)

		_, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		want := `artifact secret not found: tried "abase-test"`
		if cond.Message != want {
			t.Errorf("message = %q, want %q", cond.Message, want)
		}
	})

	t.Run("the bare base alone resolves nothing", func(t *testing.T) {
		rec, client := artifactTestReconciler(t, false)
		client.existingSecrets["abase"] = map[string][]byte{
			"access-key-id": []byte("a"), "secret-access-key": []byte("b"),
		}
		cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)
		seedClientCRDs(client)

		fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
		if fragment != "" {
			t.Errorf("expected no fragment from the bare base, got %q", fragment)
		}
		if cond.Reason != "LocalOnly" {
			t.Errorf("cond = %+v, want LocalOnly", cond)
		}
	})
}

// TestArtifactStorageFragment_ReadFaultStopsBeforePool covers task 1.10: a
// non-NotFound error on the per-cell candidate stops the lookup before the
// pool candidate is ever read. It also asserts the review fix: the
// returned error wraps errArtifactSecretRead, the sentinel
// resolveBundleForController and handleProvisioning use to tell this fault
// apart from an ordinary bundle-content fault.
func TestArtifactStorageFragment_ReadFaultStopsBeforePool(t *testing.T) {
	rec, client := artifactTestReconciler(t, false)
	client.getSecretErrs = map[string]error{"abase-cell-big-0": errors.New("etcd unavailable")}
	client.existingSecrets["abase-big"] = map[string][]byte{
		"access-key-id": []byte("a"), "secret-access-key": []byte("b"),
	}
	cr := artifactCellController("cell-big-0", "big")
	seedClientCRDs(client)

	fragment, cond, err := rec.artifactStorageFragment(context.Background(), cr)
	if fragment != "" {
		t.Errorf("expected no fragment on a read fault, got %q", fragment)
	}
	if cond.Reason != "LocalOnly" {
		t.Fatalf("cond.Reason = %q, want LocalOnly", cond.Reason)
	}
	want := `artifact secret "abase-cell-big-0" read failed: etcd unavailable`
	if cond.Message != want {
		t.Errorf("message = %q, want %q", cond.Message, want)
	}
	if !errors.Is(err, errArtifactSecretRead) {
		t.Errorf("err = %v, want it to wrap errArtifactSecretRead", err)
	}
	if client.getSecretCalls["abase-big"] != 0 {
		t.Error("expected the pool candidate never read after the per-cell fault")
	}
}

// TestArtifactStorageFragment_NotFoundFallsThroughToPool asserts the
// production path: a real apierrors.NewNotFound answer on the per-cell
// candidate (not a bare nil,nil miss) moves the lookup to the pool
// candidate, which resolves. The fake testClient.GetSecret answers a plain
// nil,nil for an unseeded name, so without this explicit NotFound error the
// apierrors.IsNotFound branch in lookupArtifactSecret is never exercised.
func TestArtifactStorageFragment_NotFoundFallsThroughToPool(t *testing.T) {
	rec, client := artifactTestReconciler(t, false)
	client.getSecretErrs = map[string]error{
		"abase-cell-big-0": apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "abase-cell-big-0"),
	}
	client.existingSecrets["abase-big"] = map[string][]byte{
		"access-key-id": []byte("a"), "secret-access-key": []byte("b"),
	}
	cr := artifactCellController("cell-big-0", "big")
	pinnedProfile(client, cr)
	seedClientCRDs(client)

	fragment, cond, _ := rec.artifactStorageFragment(context.Background(), cr)
	if fragment == "" {
		t.Fatal("expected a rendered fragment: NotFound must fall through to the pool candidate")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != "External" {
		t.Fatalf("condition = %+v, want True/External", cond)
	}
	if !strings.Contains(cond.Message, "abase-big") {
		t.Errorf("message must name the pool secret, got %q", cond.Message)
	}
	if client.getSecretCalls["abase-cell-big-0"] != 1 {
		t.Errorf("expected exactly one read of the per-cell candidate, got %d", client.getSecretCalls["abase-cell-big-0"])
	}
	if client.getSecretCalls["abase-big"] != 1 {
		t.Errorf("expected exactly one read of the pool candidate, got %d", client.getSecretCalls["abase-big"])
	}
}

// TestResolveArtifactSecret_PoolCandidate covers task 1.10: the projected
// copy carries the data of the Secret the lookup chose, here the pool one.
func TestResolveArtifactSecret_PoolCandidate(t *testing.T) {
	rec, client := artifactTestReconciler(t, false)
	client.existingSecrets["abase-big"] = map[string][]byte{
		"access-key-id": []byte("AKIAPOOL"), "secret-access-key": []byte("poolsecret"),
	}
	cr := artifactCellController("cell-big-2", "big")
	seedClientCRDs(client)

	name, hash, err := rec.resolveArtifactSecret(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveArtifactSecret: %v", err)
	}
	if name != "cell-big-2-artifacts" {
		t.Errorf("projected name = %q, want cell-big-2-artifacts", name)
	}
	if hash == "" {
		t.Error("expected a non-empty hash")
	}
	data := client.createdSecrets[name]
	if string(data["access-key-id"]) != "AKIAPOOL" {
		t.Errorf("projected data = %v, want the pool secret's data", data)
	}
}

// TestResolveArtifactSecret_ProjectionWriteFailure covers task 1.10: a
// projection write failure returns an error rather than a silent no-op.
func TestResolveArtifactSecret_ProjectionWriteFailure(t *testing.T) {
	rec, client := artifactTestReconciler(t, true)
	client.createOrUpdateSecretWithOwnerErr = errors.New("apiserver unavailable")
	cr := testController("test", "ns", v1alpha1.ControllerPhaseProvisioning)

	name, hash, err := rec.resolveArtifactSecret(context.Background(), cr)
	if err == nil {
		t.Fatal("expected an error from a projection write failure")
	}
	if name != "" || hash != "" {
		t.Errorf("expected empty name/hash on failure, got %q/%q", name, hash)
	}
}

// TestBlobstoreIsS3 covers task 1.11: an S3 URL is true, a Postgres URL and
// an unparsable URL are both false.
func TestBlobstoreIsS3(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{name: "s3", url: "s3://my-bucket?region=us-east-1", want: true},
		{name: "postgres", url: "pgblob:", want: false},
		{name: "unparsable", url: "s3://bad host/path", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BlobstoreIsS3(tc.url); got != tc.want {
				t.Errorf("BlobstoreIsS3(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

// TestVersionPinsPlugin covers task 1.11: resolution goes by version, not by
// a profile named like the pool, so a profile matching the pool's name but
// not its version never influences the answer.
func TestVersionPinsPlugin(t *testing.T) {
	rec, client := artifactTestReconciler(t, false)
	// The exact profile for the tested version lacks the pin.
	seedProfileWithoutArtifactManager(client)
	// A differently-versioned profile, named like a pool, does pin it. It
	// must never be consulted: coreSetForCr resolves by cr.Spec.Version.
	client.profiles["big"] = &v1alpha1.JenkinsVersionProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "big"},
		Spec:       v1alpha1.JenkinsVersionProfileSpec{Version: "2.999"},
		Status: v1alpha1.JenkinsVersionProfileStatus{
			ContentRef: "profile-content-big",
			Conditions: []v1alpha1.JenkinsVersionProfileCondition{{
				Type: "PluginSetReady", Status: metav1.ConditionTrue,
			}},
		},
	}
	client.configMapData["profile-content-big"] = map[string]string{
		"plugins.yaml": "plugins:\n  - artifactId: artifact-manager-s3\n    version: \"986.v\"\n",
	}
	seedClientCRDs(client)

	if got := rec.VersionPinsPlugin(context.Background(), "2.570", "artifact-manager-s3"); got {
		t.Error("expected false: the version 2.570 profile does not pin the plugin")
	}
}

// TestHandleProvisioning_ArtifactSecretReadFaultFailsTick covers task 1.12:
// a non-NotFound error on the projection's read fails the provisioning
// tick, persists status, and writes no StatefulSet. It also asserts the
// review fix that routes this fault through markReconcileBlocked, the same
// way every sibling provisioning-write failure does (CascConfigMapFailed,
// StatefulSetCreateFailed, IngressCreateFailed): ConditionReconcileBlocked
// reports True/ArtifactSecretFailed and status.lastReconcileError names the
// fault, so the attention feed and the controller detail page surface it.
func TestHandleProvisioning_ArtifactSecretReadFaultFailsTick(t *testing.T) {
	client := newTestClientWithBundle()
	client.getSecretErrs = map[string]error{"abase-test": errors.New("etcd unavailable")}
	rec := newTestReconciler(client)
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseProvisioning)

	if err := rec.handleProvisioning(context.Background(), cr); err == nil {
		t.Fatal("expected handleProvisioning to fail on a non-NotFound secret read error")
	}
	if len(client.statefulSets) != 0 {
		t.Errorf("expected no StatefulSet, got %v", client.statefulSets)
	}
	if client.lastPatchedStatus == nil {
		t.Error("expected the status to be persisted before the error return")
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonReconcileBlockedArtifactSecretFailed {
		t.Fatalf("ReconcileBlocked condition = %+v, want True/%s", cond, v1alpha1.ReasonReconcileBlockedArtifactSecretFailed)
	}
	if cr.Status.LastReconcileError == "" {
		t.Error("expected status.lastReconcileError to name the fault")
	}
	if !strings.Contains(cr.Status.LastReconcileError, "abase-test") {
		t.Errorf("expected status.lastReconcileError to name the candidate Secret, got %q", cr.Status.LastReconcileError)
	}
}

// TestProvisioningCellExhaustedCandidatesReportsArtifactStorageLocal covers
// task 1.12: a cell whose every candidate is absent (all NotFound) gets no
// StatefulSet and reports CellReady=False/ArtifactStorageLocal, driven
// through reconcileController so both handleProvisioning and
// evaluateCellReadiness run on the same tick.
func TestProvisioningCellExhaustedCandidatesReportsArtifactStorageLocal(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	rec.SetClaimGate("1")
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")

	cr := artifactCellController("cell-big-0", "big")
	pinnedProfile(client, cr)
	seedClientCRDs(client)

	if err := rec.reconcileController(context.Background(), cr); err != nil {
		t.Fatalf("reconcileController: %v", err)
	}
	if len(client.stsSpecs) != 0 {
		t.Errorf("expected no StatefulSet, got %d", len(client.stsSpecs))
	}
	cond := findCondition(cr.Status.Conditions, v1alpha1.ConditionCellReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != v1alpha1.ReasonArtifactStorageLocal {
		t.Fatalf("CellReady = %+v, want False/ArtifactStorageLocal", cond)
	}
}

// TestHandleProvisioning_CellArtifactSecretReadFaultBlocksReconcile covers
// the review fix: for a cell, cellConfigGateReason returns nil before
// resolveArtifactSecret ever runs, so a Secret read fault used to leave the
// cell sitting in Provisioning with no ReconcileBlocked and no
// lastReconcileError. A labelled cell's per-cell read failing with a
// non-NotFound error must now surface ReconcileBlocked/ArtifactSecretFailed
// from handleProvisioning directly, before the cell gate ever runs, and
// must never fall through to the pool candidate.
func TestHandleProvisioning_CellArtifactSecretReadFaultBlocksReconcile(t *testing.T) {
	client := newTestClientWithBundle()
	client.getSecretErrs = map[string]error{"abase-cell-big-0": errors.New("etcd unavailable")}
	rec := newTestReconciler(client)
	rec.SetClaimGate("1")
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")

	cr := artifactCellController("cell-big-0", "big")

	if err := rec.handleProvisioning(context.Background(), cr); err == nil {
		t.Fatal("expected handleProvisioning to fail on a non-NotFound secret read error")
	}
	if len(client.statefulSets) != 0 {
		t.Errorf("expected no StatefulSet, got %v", client.statefulSets)
	}

	artifactCond := findCondition(cr.Status.Conditions, ConditionArtifactStorage)
	if artifactCond == nil || artifactCond.Status != metav1.ConditionFalse || artifactCond.Reason != "LocalOnly" {
		t.Fatalf("ArtifactStorage condition = %+v, want False/LocalOnly", artifactCond)
	}
	wantMsg := `artifact secret "abase-cell-big-0" read failed: etcd unavailable`
	if artifactCond.Message != wantMsg {
		t.Errorf("ArtifactStorage message = %q, want %q", artifactCond.Message, wantMsg)
	}

	blockedCond := findCondition(cr.Status.Conditions, v1alpha1.ConditionReconcileBlocked)
	if blockedCond == nil || blockedCond.Status != metav1.ConditionTrue || blockedCond.Reason != v1alpha1.ReasonReconcileBlockedArtifactSecretFailed {
		t.Fatalf("ReconcileBlocked condition = %+v, want True/%s", blockedCond, v1alpha1.ReasonReconcileBlockedArtifactSecretFailed)
	}
	if cr.Status.LastReconcileError == "" {
		t.Error("expected status.lastReconcileError to name the fault")
	}
	if client.getSecretCalls["abase-big"] != 0 {
		t.Error("expected the pool candidate never read after the per-cell fault")
	}
}

// TestCheckPluginRollFailed_ArtifactSecretReadFaultGivesNoVerdict covers a
// follow-on review fix: checkPluginRollFailed must not compute a plugin-roll
// verdict from a bundle whose artifact-storage step failed to read its
// Secret, because that verdict would be missing the bundle's own plugin
// pins. A pre-existing PluginRollFailed condition stays exactly as it was
// while the fault lasts, and the check resumes its normal verdict once the
// fault clears.
func TestCheckPluginRollFailed_ArtifactSecretReadFaultGivesNoVerdict(t *testing.T) {
	client := newTestClientWithBundle()
	client.configMapData["test-bundle-content"]["plugins.yaml"] = "plugins:\n  - artifactId: workflow-aggregator\n    version: latest\n"
	client.getSecretErrs = map[string]error{"abase-test": errors.New("etcd unavailable")}
	rec := newTestReconciler(client)
	rec.SetArtifactSecretBase("abase")
	rec.SetBlobstoreURL("s3://my-bucket?region=us-east-1")

	cr := testController("test", "ns1", v1alpha1.ControllerPhaseRunning)
	existing := v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPluginRollFailed,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.ReasonPluginRollFailed,
		Message: "a prior roll failed",
	}
	cr.Status.Conditions = []v1alpha1.ControllerCondition{existing}

	rec.checkPluginRollFailed(context.Background(), cr, nil, testLogger())

	got := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginRollFailed)
	if got == nil || got.Status != existing.Status || got.Reason != existing.Reason || got.Message != existing.Message {
		t.Fatalf("PluginRollFailed changed during a Secret read fault: got %+v, want unchanged %+v", got, existing)
	}

	// The fault is gone: the check runs its normal verdict again and
	// clears the condition, the same way it did before this fix.
	delete(client.getSecretErrs, "abase-test")
	rec.checkPluginRollFailed(context.Background(), cr, nil, testLogger())
	cleared := findCondition(cr.Status.Conditions, v1alpha1.ConditionPluginRollFailed)
	if cleared == nil || cleared.Status != metav1.ConditionFalse || cleared.Reason != "RollComplete" {
		t.Fatalf("expected PluginRollFailed to clear once the fault is gone, got %+v", cleared)
	}
}

// TestSecretDataHash covers the framing of a Secret's values. A NUL byte
// inside a value must not look like a key or value boundary, and binary
// values must hash by their bytes.
func TestSecretDataHash(t *testing.T) {
	t.Run("delimiter collision differs", func(t *testing.T) {
		a := secretDataHash(map[string][]byte{"a": []byte("x\x00b\x00y")})
		b := secretDataHash(map[string][]byte{"a": []byte("x"), "b": []byte("y")})
		if a == b {
			t.Fatal("a NUL byte in a value collides with two keys")
		}
	})
	t.Run("key order is irrelevant", func(t *testing.T) {
		first := map[string][]byte{}
		first["a"] = []byte("1")
		first["b"] = []byte("2")
		first["c"] = []byte("3")
		second := map[string][]byte{}
		second["c"] = []byte("3")
		second["b"] = []byte("2")
		second["a"] = []byte("1")
		want := secretDataHash(first)
		for i := 0; i < 50; i++ {
			if got := secretDataHash(first); got != want {
				t.Fatalf("iteration %d: hash %q, want %q", i, got, want)
			}
			if got := secretDataHash(second); got != want {
				t.Fatalf("iteration %d: reordered hash %q, want %q", i, got, want)
			}
		}
	})
	t.Run("value change changes hash", func(t *testing.T) {
		a := secretDataHash(map[string][]byte{"a": []byte("one"), "b": []byte("two")})
		b := secretDataHash(map[string][]byte{"a": []byte("one"), "b": []byte("twp")})
		if a == b {
			t.Fatal("changing a value did not change the hash")
		}
	})
	t.Run("binary values differ", func(t *testing.T) {
		a := secretDataHash(map[string][]byte{"a": {0xff}})
		b := secretDataHash(map[string][]byte{"a": {0xfe}})
		if a == b {
			t.Fatal("distinct invalid UTF-8 values hash the same")
		}
	})
	t.Run("empty answers empty", func(t *testing.T) {
		if got := secretDataHash(nil); got != "" {
			t.Errorf("nil hash = %q, want empty", got)
		}
		if got := secretDataHash(map[string][]byte{}); got != "" {
			t.Errorf("empty map hash = %q, want empty", got)
		}
	})
}

// TestSecretDataHash_GoldenValue pins the encoding. Changing the encoding
// rolls every pod that carries a static artifact Secret once, the same
// way TestComputeDesiredContentHash_GoldenValue pins the content hash.
func TestSecretDataHash_GoldenValue(t *testing.T) {
	got := secretDataHash(map[string][]byte{
		"access-key-id":     []byte("AKIAEXAMPLE"),
		"secret-access-key": []byte("secretvalue"),
	})
	const want = "785f9441c0fcd3ccf85b07554314a64b4d6e5c463c580d845e4497ba31700ea0"
	if got != want {
		t.Errorf("secretDataHash = %s, want %s", got, want)
	}
}
