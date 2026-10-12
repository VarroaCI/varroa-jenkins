package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// poolMailCoreSet pins the two plugins a pool mail setting needs.
func poolMailCoreSet() []pluginlock.PluginEntry {
	return []pluginlock.PluginEntry{
		{ArtifactID: "mailer", Version: "1.0"},
		{ArtifactID: "email-ext", Version: "2038.v7b_8817a_499d9"},
	}
}

// poolMailSecret returns a labelled mail login Secret as ListSecrets shapes it.
func poolMailSecret(name, username, password string) map[string][]byte {
	return poolLibrarySecret(name, map[string]string{"username": username, "password": password})
}

// poolMailSpec returns a mail setting that names a login Secret.
func poolMailSpec(credentialRef string) *v1alpha1.CellPoolMail {
	return &v1alpha1.CellPoolMail{
		Host:          "smtp.example.com",
		Port:          465,
		Security:      "tls",
		From:          "ci@example.com",
		ReplyTo:       "ops@example.com",
		DefaultSuffix: "@example.com",
		CredentialRef: credentialRef,
	}
}

// mailerOf returns the mailer map of a rendered set.
func mailerOf(t *testing.T, u map[string]any) map[string]any {
	t.Helper()
	m, ok := u["mailer"].(map[string]any)
	if !ok {
		t.Fatalf("rendered set has no mailer map: %v", u)
	}
	return m
}

func TestRenderPoolMail_NoCredential(t *testing.T) {
	m := &v1alpha1.CellPoolMail{Host: "smtp.example.com", From: "ci@example.com"}
	set := renderPoolMail(m, nil, nil, poolMailCoreSet())
	if set.Reason != ReasonPoolMailRendered || len(set.Faults) != 0 {
		t.Fatalf("reason=%q faults=%v, want Rendered and no faults", set.Reason, set.Faults)
	}
	mailer := mailerOf(t, set.Unclassified)
	if _, ok := mailer["authentication"]; ok {
		t.Error("a mail setting with no credentialRef must carry no authentication key")
	}
	if mailer["smtpPort"] != "587" {
		t.Errorf("smtpPort = %v, want the default 587", mailer["smtpPort"])
	}
	loc := set.Unclassified["location"].(map[string]any)
	if loc["adminAddress"] != "ci@example.com" {
		t.Errorf("location.adminAddress = %v", loc["adminAddress"])
	}
}

func TestRenderPoolMail_UsernamePasswordInline(t *testing.T) {
	secrets := []map[string][]byte{poolMailSecret("mail-login", "svc", "PW-VALUE")}
	set := renderPoolMail(poolMailSpec("mail-login"), secrets, nil, poolMailCoreSet())
	if set.Reason != ReasonPoolMailRendered || len(set.Faults) != 0 {
		t.Fatalf("reason=%q faults=%v", set.Reason, set.Faults)
	}
	mailer := mailerOf(t, set.Unclassified)
	auth, ok := mailer["authentication"].(map[string]any)
	if !ok || auth["username"] != "svc" || auth["password"] != "PW-VALUE" {
		t.Fatalf("mailer authentication = %v", mailer["authentication"])
	}
	if mailer["replyToAddress"] != "ops@example.com" || mailer["defaultSuffix"] != "@example.com" {
		t.Errorf("mailer reply-to or suffix missing: %v", mailer)
	}
	ext := set.Unclassified["email-ext"].(map[string]any)
	account := ext["mailAccount"].(map[string]any)
	if _, has := account["credentialsId"]; has {
		t.Error("email-ext must carry no credentialsId")
	}
	if account["address"] != "ci@example.com" || ext["defaultReplyTo"] != "ops@example.com" {
		t.Errorf("email-ext account or reply-to wrong: %v", ext)
	}
}

func TestRenderPoolMail_SecurityModes(t *testing.T) {
	cases := []struct {
		security       string
		useSsl, useTLS bool
	}{
		{"none", false, false},
		{"starttls", false, true},
		{"tls", true, false},
		{"", false, true},
	}
	for _, tc := range cases {
		m := &v1alpha1.CellPoolMail{Host: "smtp.example.com", From: "ci@example.com", Security: tc.security}
		set := renderPoolMail(m, nil, nil, poolMailCoreSet())
		mailer := mailerOf(t, set.Unclassified)
		if mailer["useSsl"] != tc.useSsl || mailer["useTls"] != tc.useTLS {
			t.Errorf("security %q: useSsl=%v useTls=%v, want %v %v",
				tc.security, mailer["useSsl"], mailer["useTls"], tc.useSsl, tc.useTLS)
		}
	}
}

func TestRenderPoolMail_CredentialMissingRendersNothing(t *testing.T) {
	set := renderPoolMail(poolMailSpec("mail-login"), nil, nil, poolMailCoreSet())
	if set.Reason != ReasonPoolMailCredentialMissing || len(set.Faults) == 0 {
		t.Fatalf("reason=%q faults=%v, want CredentialMissing", set.Reason, set.Faults)
	}
	if set.Unclassified != nil {
		t.Errorf("a missing login must render nothing, got %v", set.Unclassified)
	}

	set = renderPoolMail(poolMailSpec("mail-login"), nil, errors.New("list failed"), poolMailCoreSet())
	if set.Reason != ReasonPoolMailCredentialMissing || set.Unclassified != nil {
		t.Errorf("a list error must render nothing: reason=%q unclassified=%v", set.Reason, set.Unclassified)
	}
}

func TestRenderPoolMail_CredentialMalformed(t *testing.T) {
	cases := map[string]map[string][]byte{
		"no username": poolLibrarySecret("mail-login", map[string]string{"password": "pw"}),
		"var token":   poolMailSecret("mail-login", "svc", "${secret}"),
	}
	for name, data := range cases {
		set := renderPoolMail(poolMailSpec("mail-login"), []map[string][]byte{data}, nil, poolMailCoreSet())
		if set.Reason != ReasonPoolMailCredentialMalformed {
			t.Errorf("%s: reason=%q, want CredentialMalformed", name, set.Reason)
		}
		if set.Unclassified != nil {
			t.Errorf("%s: a malformed login must render nothing", name)
		}
	}
}

func TestRenderPoolMail_EmailExtNotPinnedKeepsMailer(t *testing.T) {
	core := []pluginlock.PluginEntry{{ArtifactID: "mailer", Version: "1.0"}}
	set := renderPoolMail(poolMailSpec(""), nil, nil, core)
	if set.Reason != ReasonPoolMailPluginMissing || len(set.Faults) == 0 {
		t.Fatalf("reason=%q faults=%v, want PluginMissing", set.Reason, set.Faults)
	}
	mailerOf(t, set.Unclassified)
	if _, ok := set.Unclassified["email-ext"]; ok {
		t.Error("an unpinned email-ext must not render")
	}
}

func TestRenderPoolMail_MailerNotPinnedRendersNothing(t *testing.T) {
	core := []pluginlock.PluginEntry{{ArtifactID: "email-ext", Version: "2038.v7b_8817a_499d9"}}
	set := renderPoolMail(poolMailSpec(""), nil, nil, core)
	if set.Reason != ReasonPoolMailPluginMissing || set.Unclassified != nil {
		t.Fatalf("an unpinned mailer must render nothing: reason=%q unclassified=%v", set.Reason, set.Unclassified)
	}
}

func TestInjectPoolMail_KeepsLocationURL(t *testing.T) {
	jcasc := "unclassified:\n  location:\n    url: https://cell.example.com/\n"
	keys := map[string]any{
		"location": map[string]any{"adminAddress": "ci@example.com"},
	}
	out, err := injectPoolMail(jcasc, keys)
	if err != nil {
		t.Fatalf("injectPoolMail: %v", err)
	}
	var doc struct {
		Unclassified struct {
			Location map[string]any `yaml:"location"`
		} `yaml:"unclassified"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("parse output: %v", err)
	}
	if doc.Unclassified.Location["url"] != "https://cell.example.com/" {
		t.Errorf("location.url was lost: %v", doc.Unclassified.Location)
	}
	if doc.Unclassified.Location["adminAddress"] != "ci@example.com" {
		t.Errorf("location.adminAddress not merged: %v", doc.Unclassified.Location)
	}
}

func TestInjectPoolMail_PoolWinsOverBundleMailer(t *testing.T) {
	jcasc := "unclassified:\n  mailer:\n    smtpHost: old.example.com\n    smtpPort: \"25\"\n"
	keys := map[string]any{
		"mailer": map[string]any{"smtpHost": "new.example.com", "smtpPort": "587"},
	}
	out, err := injectPoolMail(jcasc, keys)
	if err != nil {
		t.Fatalf("injectPoolMail: %v", err)
	}
	if strings.Contains(out, "old.example.com") || !strings.Contains(out, "new.example.com") {
		t.Errorf("the pool mailer must replace the bundle mailer:\n%s", out)
	}
}

func TestInjectPoolMail_EmptyKeysUnchanged(t *testing.T) {
	jcasc := "jenkins:\n  systemMessage: hi\n"
	out, err := injectPoolMail(jcasc, nil)
	if err != nil || out != jcasc {
		t.Fatalf("empty keys must return the input unchanged: out=%q err=%v", out, err)
	}
}

func TestApplyPoolMail_NonCellNoop(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := testController("plain", "ns1", v1alpha1.ControllerPhaseConnected)
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}

	if err := rec.applyPoolMail(context.Background(), cr, mat, poolMailCoreSet()); err != nil {
		t.Fatalf("applyPoolMail: %v", err)
	}
	if mat.CellPoolMail != nil {
		t.Fatalf("a non-cell controller must not set pool mail: %v", mat.CellPoolMail)
	}
}

// seedPoolMailPool seeds pool-a with a mail setting and the cell label.
func seedPoolMailPool(client *testClient, cr *v1alpha1.Controller, mail *v1alpha1.CellPoolMail) {
	crdstore.MustSeed(client.store, &v1alpha1.CellPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
		Spec: v1alpha1.CellPoolSpec{
			ProfileRef: "lts-2-555",
			Mail:       mail,
		},
	})
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
}

// seedPoolMailSecret registers a labelled mail login Secret in the operator
// namespace.
func seedPoolMailSecret(client *testClient, name, password string) {
	client.labeledSecrets = append(client.labeledSecrets, labeledSecretFixture{
		name: name, namespace: "varroa-system",
		labels: map[string]string{"varroa.dev/pool-mail-credential": "true"},
	})
	client.existingSecrets[name] = map[string][]byte{
		"username": []byte("svc"), "password": []byte(password),
	}
}

// TestApplyPoolMail_SetsAndClearsCondition checks the PoolMail condition
// across three passes: a rendered setting, a held setting whose Secret is
// missing, and a pool that no longer declares a setting.
func TestApplyPoolMail_SetsAndClearsCondition(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	seedPoolMailPool(client, cr, poolMailSpec("mail-login"))
	seedPoolMailSecret(client, "mail-login", "pw")

	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolMail(context.Background(), cr, mat, poolMailCoreSet()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	cond := findControllerCondition(cr, v1alpha1.ConditionPoolMail)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonPoolMailRendered {
		t.Fatalf("pass 1 condition = %+v, want True/Rendered", cond)
	}
	if mat.CellPoolMail == nil {
		t.Fatal("pass 1 must set the pool mail keys")
	}

	delete(client.existingSecrets, "mail-login")
	client.labeledSecrets = nil
	mat = &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolMail(context.Background(), cr, mat, poolMailCoreSet()); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	cond = findControllerCondition(cr, v1alpha1.ConditionPoolMail)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonPoolMailCredentialMissing {
		t.Fatalf("pass 2 condition = %+v, want False/CredentialMissing", cond)
	}
	if mat.CellPoolMail != nil {
		t.Fatalf("a held setting must not render: %v", mat.CellPoolMail)
	}

	cr.Labels = map[string]string{cells.LabelCellPool: "gone"}
	mat = &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolMail(context.Background(), cr, mat, poolMailCoreSet()); err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if findControllerCondition(cr, v1alpha1.ConditionPoolMail) != nil {
		t.Fatal("pass 3 must remove the PoolMail condition")
	}
}

// TestApplyPoolMail_PoolReadErrorFails checks that a pool read failure is an
// error and never reads as a pool with no mail setting.
func TestApplyPoolMail_PoolReadErrorFails(t *testing.T) {
	client := newTestClient()
	rec := newTestReconciler(client)
	cr := cellController()
	cr.Labels = map[string]string{cells.LabelCellPool: "pool-a"}
	gvr := schema.GroupVersionResource{Group: "varroa.dev", Version: "v1alpha1", Resource: "cellpools"}
	client.store.FailAlways("get", gvr, errors.New("apiserver unavailable"))

	mat := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hi\n"}
	if err := rec.applyPoolMail(context.Background(), cr, mat, poolMailCoreSet()); err == nil {
		t.Fatal("a pool read failure must fail the resolve")
	}
}

// TestBuildDesiredStateCommand_PoolMailPasswordOnlyInMiteJcasc checks that the
// SMTP login reaches the mite command only, and that a rotated login changes
// the desired-state hash.
func TestBuildDesiredStateCommand_PoolMailPasswordOnlyInMiteJcasc(t *testing.T) {
	client := newTestClientWithBundle()
	rec := newTestReconciler(client)
	cr := cellController()
	seedPoolMailPool(client, cr, poolMailSpec("mail-login"))
	seedPoolMailSecret(client, "mail-login", "PW-OLD-VALUE")

	build := func() (*bundle.MaterializedBundle, *mitev1.DesiredStateCommand) {
		t.Helper()
		resolved := &bundle.MaterializedBundle{JenkinsYAML: "jenkins:\n  systemMessage: hello"}
		if err := rec.applyPoolMail(context.Background(), cr, resolved, poolMailCoreSet()); err != nil {
			t.Fatalf("applyPoolMail: %v", err)
		}
		cmd, err := rec.buildDesiredStateCommand(cr, resolved, runners.RunnerContribution{})
		if err != nil {
			t.Fatalf("buildDesiredStateCommand: %v", err)
		}
		return resolved, cmd
	}

	resolved1, cmd1 := build()
	if !strings.Contains(cmd1.JcascYaml, "PW-OLD-VALUE") {
		t.Error("JcascYaml does not carry the pool mail password")
	}
	if strings.Contains(resolved1.JenkinsYAML, "PW-OLD-VALUE") {
		t.Error("resolved.JenkinsYAML (the ConfigMap content) must not carry the pool mail password")
	}

	client.existingSecrets["mail-login"] = map[string][]byte{
		"username": []byte("svc"), "password": []byte("PW-ROTATED-VALUE"),
	}
	_, cmd2 := build()
	if strings.Contains(cmd2.JcascYaml, "PW-OLD-VALUE") || !strings.Contains(cmd2.JcascYaml, "PW-ROTATED-VALUE") {
		t.Error("the rotated password must reach JcascYaml and the old one must not")
	}
	if cmd1.DesiredStateHash == cmd2.DesiredStateHash {
		t.Error("rotating the pool mail login did not change the desired-state hash")
	}
}
