package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// poolLibraryCoreSet pins every plugin a pool library needs.
func poolLibraryCoreSet() []pluginlock.PluginEntry {
	return []pluginlock.PluginEntry{
		{ArtifactID: "pipeline-groovy-lib", Version: "805.va_fc79344957d"},
		{ArtifactID: "git", Version: "5.10.1"},
	}
}

// poolLibrarySecret returns a labelled Secret record as ListSecrets shapes it.
func poolLibrarySecret(name string, data map[string]string) map[string][]byte {
	rec := map[string][]byte{"_name": []byte(name)}
	for k, v := range data {
		rec[k] = []byte(v)
	}
	return rec
}

func TestRenderPoolLibraries_HTTPSNoCredential(t *testing.T) {
	libs := []v1alpha1.CellPoolLibrary{{
		Name: "platform-lib", Remote: "https://example.com/platform.git", DefaultVersion: "main", Implicit: true,
	}}
	set := renderPoolLibraries(libs, nil, nil, poolLibraryCoreSet())
	if set.Reason != ReasonPoolLibrariesRendered || len(set.Faults) != 0 {
		t.Fatalf("reason=%q faults=%v, want Rendered and none", set.Reason, set.Faults)
	}
	if len(set.Entries) != 1 || len(set.Credentials) != 0 {
		t.Fatalf("entries=%d credentials=%d, want 1 and 0", len(set.Entries), len(set.Credentials))
	}
	entry := set.Entries[0].(map[string]any)
	if entry["name"] != "platform-lib" || entry["implicit"] != true || entry["allowVersionOverride"] != false {
		t.Fatalf("entry = %v", entry)
	}
	git := entry["retriever"].(map[string]any)["modernSCM"].(map[string]any)["scm"].(map[string]any)["git"].(map[string]any)
	if _, has := git["credentialsId"]; has {
		t.Fatalf("https library without credentialRef must carry no credentialsId: %v", git)
	}
}

func TestRenderPoolLibraries_UsernamePasswordCredential(t *testing.T) {
	libs := []v1alpha1.CellPoolLibrary{{
		Name: "platform-lib", Remote: "https://example.com/platform.git", DefaultVersion: "main", CredentialRef: "lib-login",
	}}
	secrets := []map[string][]byte{poolLibrarySecret("lib-login", map[string]string{"username": "svc", "password": "pw"})}
	set := renderPoolLibraries(libs, secrets, nil, poolLibraryCoreSet())
	if set.Reason != ReasonPoolLibrariesRendered {
		t.Fatalf("reason = %q, faults %v", set.Reason, set.Faults)
	}
	cred := set.Credentials[0].(map[string]any)["usernamePassword"].(map[string]any)
	if cred["id"] != "varroa-pool-library-platform-lib" || cred["scope"] != "SYSTEM" || cred["password"] != "pw" {
		t.Fatalf("credential = %v", cred)
	}
	git := set.Entries[0].(map[string]any)["retriever"].(map[string]any)["modernSCM"].(map[string]any)["scm"].(map[string]any)["git"].(map[string]any)
	if git["credentialsId"] != "varroa-pool-library-platform-lib" {
		t.Fatalf("credentialsId = %v", git["credentialsId"])
	}
}

func TestRenderPoolLibraries_SSHKeyWithPassphrase(t *testing.T) {
	libs := []v1alpha1.CellPoolLibrary{{
		Name: "platform-lib", Remote: "ssh://git@example.com/platform.git", DefaultVersion: "main", CredentialRef: "lib-key",
	}}
	secrets := []map[string][]byte{poolLibrarySecret("lib-key", map[string]string{
		"username": "git", "privateKey": "KEY", "passphrase": "phrase",
	})}
	set := renderPoolLibraries(libs, secrets, nil, poolLibraryCoreSet())
	if set.Reason != ReasonPoolLibrariesRendered {
		t.Fatalf("reason = %q, faults %v", set.Reason, set.Faults)
	}
	key := set.Credentials[0].(map[string]any)["basicSSHUserPrivateKey"].(map[string]any)
	if key["passphrase"] != "phrase" || key["scope"] != "SYSTEM" {
		t.Fatalf("ssh credential = %v", key)
	}
	src := key["privateKeySource"].(map[string]any)["directEntry"].(map[string]any)
	if src["privateKey"] != "KEY" {
		t.Fatalf("private key = %v", src)
	}
}

func TestRenderPoolLibraries_CredentialMissingHoldsOnlyThatLibrary(t *testing.T) {
	libs := []v1alpha1.CellPoolLibrary{
		{Name: "held", Remote: "https://example.com/a.git", DefaultVersion: "main", CredentialRef: "absent"},
		{Name: "open", Remote: "https://example.com/b.git", DefaultVersion: "main"},
	}
	set := renderPoolLibraries(libs, nil, nil, poolLibraryCoreSet())
	if set.Reason != ReasonPoolLibraryCredentialMissing {
		t.Fatalf("reason = %q", set.Reason)
	}
	if len(set.Entries) != 1 || set.Entries[0].(map[string]any)["name"] != "open" {
		t.Fatalf("only the open library should render, got %v", set.Entries)
	}
}

func TestRenderPoolLibraries_CredentialMalformed(t *testing.T) {
	cases := map[string]map[string]string{
		"no username":              {"password": "pw"},
		"password and privateKey":  {"username": "u", "password": "pw", "privateKey": "K"},
		"neither password nor key": {"username": "u"},
		"variable token":           {"username": "u", "password": "${secret}"},
		"passphrase with password": {"username": "u", "password": "pw", "passphrase": "x"},
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			libs := []v1alpha1.CellPoolLibrary{{
				Name: "platform-lib", Remote: "https://example.com/a.git", DefaultVersion: "main", CredentialRef: "login",
			}}
			secrets := []map[string][]byte{poolLibrarySecret("login", data)}
			set := renderPoolLibraries(libs, secrets, nil, poolLibraryCoreSet())
			if set.Reason != ReasonPoolLibraryCredentialMalformed {
				t.Fatalf("reason = %q, want CredentialMalformed", set.Reason)
			}
			if len(set.Entries) != 0 || len(set.Credentials) != 0 {
				t.Fatalf("a malformed credential must hold the library")
			}
		})
	}
}

func TestRenderPoolLibraries_ListErrorHoldsCredentialedLibraries(t *testing.T) {
	libs := []v1alpha1.CellPoolLibrary{
		{Name: "needs-login", Remote: "https://example.com/a.git", DefaultVersion: "main", CredentialRef: "login"},
		{Name: "anonymous", Remote: "https://example.com/b.git", DefaultVersion: "main"},
	}
	set := renderPoolLibraries(libs, nil, errors.New("apiserver unavailable"), poolLibraryCoreSet())
	if set.Reason != ReasonPoolLibraryCredentialMissing {
		t.Fatalf("reason = %q", set.Reason)
	}
	if len(set.Entries) != 1 || set.Entries[0].(map[string]any)["name"] != "anonymous" {
		t.Fatalf("entries = %v", set.Entries)
	}
}

func TestRenderPoolLibraries_PluginMissingRendersNothing(t *testing.T) {
	libs := []v1alpha1.CellPoolLibrary{{Name: "platform-lib", Remote: "https://example.com/a.git", DefaultVersion: "main"}}
	core := []pluginlock.PluginEntry{{ArtifactID: "git", Version: "5.10.1"}}
	set := renderPoolLibraries(libs, nil, nil, core)
	if set.Reason != ReasonPoolLibraryPluginMissing {
		t.Fatalf("reason = %q", set.Reason)
	}
	if len(set.Entries) != 0 {
		t.Fatalf("no library may render without pipeline-groovy-lib, got %v", set.Entries)
	}
}

func TestRenderPoolLibraries_RejectsVarTokenInRemote(t *testing.T) {
	libs := []v1alpha1.CellPoolLibrary{
		{Name: "bad", Remote: "https://example.com/${repo}.git", DefaultVersion: "main"},
		{Name: "good", Remote: "https://example.com/good.git", DefaultVersion: "main"},
	}
	set := renderPoolLibraries(libs, nil, nil, poolLibraryCoreSet())
	if set.Reason != ReasonPoolLibraryInvalid {
		t.Fatalf("reason = %q", set.Reason)
	}
	if len(set.Entries) != 1 || set.Entries[0].(map[string]any)["name"] != "good" {
		t.Fatalf("entries = %v", set.Entries)
	}
}

func TestInjectPoolLibraries_KeepsBundleLibrariesAndAppends(t *testing.T) {
	base := `unclassified:
  globalLibraries:
    libraries:
    - name: bundle-lib
      defaultVersion: v1
    - name: shared
      defaultVersion: old
`
	pool := []any{map[string]any{"name": "shared", "defaultVersion": "new"}, map[string]any{"name": "platform", "defaultVersion": "main"}}
	out, err := injectPoolLibraries(base, pool)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	names := libraryNames(t, out)
	if strings.Join(names, ",") != "bundle-lib,shared,platform" {
		t.Fatalf("names = %v", names)
	}
}

func TestInjectPoolLibraries_PoolWinsOnName(t *testing.T) {
	base := `unclassified:
  globalLibraries:
    libraries:
    - name: shared
      defaultVersion: old
`
	pool := []any{map[string]any{"name": "shared", "defaultVersion": "new"}}
	out, err := injectPoolLibraries(base, pool)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	libs := doc["unclassified"].(map[string]any)["globalLibraries"].(map[string]any)["libraries"].([]any)
	if len(libs) != 1 || libs[0].(map[string]any)["defaultVersion"] != "new" {
		t.Fatalf("libraries = %v", libs)
	}
}

func TestInjectPoolLibraries_EmptySetRendersEmptyList(t *testing.T) {
	out, err := injectPoolLibraries("jenkins:\n  systemMessage: hi\n", nil)
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !strings.Contains(out, "libraries: []") {
		t.Fatalf("an empty set must render an empty list so a removed library unloads, got:\n%s", out)
	}
}

// libraryNames returns the library names of a rendered jenkins.yaml.
func libraryNames(t *testing.T, doc string) []string {
	t.Helper()
	var root map[string]any
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatalf("parse: %v", err)
	}
	libs := root["unclassified"].(map[string]any)["globalLibraries"].(map[string]any)["libraries"].([]any)
	names := make([]string, 0, len(libs))
	for _, l := range libs {
		names = append(names, l.(map[string]any)["name"].(string))
	}
	return names
}

// poolLibrarySeedPool seeds a CellPool named pool-a with one library that
// names the Secret login-secret, and labels cr as a member of the pool.
func poolLibrarySeedPool(t *testing.T, client *testClient, cr *v1alpha1.Controller, credentialRef string) {
	t.Helper()
	crdstore.MustSeed(client.store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef: "lts-2-555",
			Libraries: []v1alpha1.CellPoolLibrary{{
				Name: "platform-lib", Remote: "https://example.com/platform.git",
				DefaultVersion: "main", CredentialRef: credentialRef,
			}},
		},
	})
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
}

// seedPoolLibrarySecret registers a labelled pool-library login Secret in
// the operator namespace, with the usual username and password keys.
func seedPoolLibrarySecret(client *testClient, name, password string) {
	client.labeledSecrets = append(client.labeledSecrets, labeledSecretFixture{
		name: name, namespace: "varroa-system",
		labels: map[string]string{"varroa.dev/pool-library-credential": "true"},
	})
	client.existingSecrets[name] = map[string][]byte{
		"username": []byte("svc"), "password": []byte(password),
	}
}

// TestApplyPoolLibraries_NonCellNoop checks that a controller that is not
// a cell leaves the bundle and its conditions unchanged.
func TestApplyPoolLibraries_NonCellNoop(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("plain", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	before := mat.JenkinsYAML

	if err := rec.applyPoolLibraries(context.Background(), cr, mat, poolLibraryCoreSet()); err != nil {
		t.Fatalf("applyPoolLibraries: %v", err)
	}
	if mat.JenkinsYAML != before || mat.CellPoolLibraryCredentials != nil {
		t.Fatalf("a non-cell controller must not change the bundle: %q", mat.JenkinsYAML)
	}
}

// TestApplyPoolLibraries_SetsAndClearsCondition checks the PoolLibraries
// condition across three passes: a rendered library, a held library whose
// Secret is missing, and a pool that no longer declares a library.
func TestApplyPoolLibraries_SetsAndClearsCondition(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	poolLibrarySeedPool(t, client, cr, "login-secret")
	seedPoolLibrarySecret(client, "login-secret", "pw")

	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolLibraries(context.Background(), cr, mat, poolLibraryCoreSet()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	cond := findControllerCondition(cr, v1alpha1.ConditionPoolLibraries)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonPoolLibrariesRendered {
		t.Fatalf("pass 1 condition = %+v, want True/Rendered", cond)
	}
	if got := libraryNames(t, mat.JenkinsYAML); len(got) != 1 || got[0] != "platform-lib" {
		t.Fatalf("pass 1 libraries = %v", got)
	}
	if len(mat.CellPoolLibraryCredentials) != 1 {
		t.Fatalf("pass 1 credentials = %d, want 1", len(mat.CellPoolLibraryCredentials))
	}

	// Pass 2: the Secret is gone, so the library is held and the condition
	// turns False with the missing-credential reason.
	delete(client.existingSecrets, "login-secret")
	client.labeledSecrets = nil
	mat = &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolLibraries(context.Background(), cr, mat, poolLibraryCoreSet()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	cond = findControllerCondition(cr, v1alpha1.ConditionPoolLibraries)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonPoolLibraryCredentialMissing {
		t.Fatalf("pass 2 condition = %+v, want False/CredentialMissing", cond)
	}
	if got := libraryNames(t, mat.JenkinsYAML); len(got) != 0 {
		t.Fatalf("a held library must not render, got %v", got)
	}

	// Pass 3: the cell's pool no longer exists, so the pool declares no
	// library. The condition is removed and the libraries key is empty.
	cr.Labels = map[string]string{cells.LabelCellPool: "gone"}
	mat = &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolLibraries(context.Background(), cr, mat, poolLibraryCoreSet()); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if findControllerCondition(cr, v1alpha1.ConditionPoolLibraries) != nil {
		t.Fatalf("pass 3 must remove the PoolLibraries condition")
	}
	if !strings.Contains(mat.JenkinsYAML, "libraries: []") {
		t.Fatalf("pass 3 must still render the empty libraries key:\n%s", mat.JenkinsYAML)
	}
}

// TestBuildDesiredStateCommand_PoolLibraryCredentialOnlyInMiteJcasc checks
// that a pool library login reaches the mite command only, and that a
// rotated login changes the desired-state hash.
func TestBuildDesiredStateCommand_PoolLibraryCredentialOnlyInMiteJcasc(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellController()
	poolLibrarySeedPool(t, client, cr, "login-secret")
	seedPoolLibrarySecret(client, "login-secret", "PW-OLD-VALUE")

	build := func() (*bundle.MaterializedBundle, *mitev1.DesiredStateCommand) {
		t.Helper()
		resolved := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hello"}
		if err := rec.applyPoolLibraries(context.Background(), cr, resolved, poolLibraryCoreSet()); err != nil {
			t.Fatalf("applyPoolLibraries: %v", err)
		}
		cmd, err := rec.buildDesiredStateCommand(cr, resolved, runners.RunnerContribution{})
		if err != nil {
			t.Fatalf("buildDesiredStateCommand: %v", err)
		}
		return resolved, cmd
	}

	resolved1, cmd1 := build()
	if !strings.Contains(cmd1.JcascYaml, "PW-OLD-VALUE") {
		t.Error("JcascYaml does not carry the pool library password")
	}
	if strings.Contains(resolved1.JenkinsYAML, "PW-OLD-VALUE") {
		t.Error("resolved.JenkinsYAML (the ConfigMap content) must not carry the pool library password")
	}

	client.existingSecrets["login-secret"] = map[string][]byte{
		"username": []byte("svc"), "password": []byte("PW-ROTATED-VALUE"),
	}
	_, cmd2 := build()
	if strings.Contains(cmd2.JcascYaml, "PW-OLD-VALUE") || !strings.Contains(cmd2.JcascYaml, "PW-ROTATED-VALUE") {
		t.Error("the rotated password must reach JcascYaml and the old one must not")
	}
	if cmd1.DesiredStateHash == cmd2.DesiredStateHash {
		t.Error("rotating the pool library login did not change the desired-state hash")
	}
}

// findControllerCondition returns the condition of cr with the given type,
// or nil.
func findControllerCondition(cr *v1alpha1.Controller, ctype v1alpha1.ControllerConditionType) *v1alpha1.ControllerCondition {
	for i := range cr.Status.Conditions {
		if cr.Status.Conditions[i].Type == ctype {
			return &cr.Status.Conditions[i]
		}
	}
	return nil
}
