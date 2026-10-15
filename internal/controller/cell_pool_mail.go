package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// poolMailCredentialLabelSelector marks a Secret as the SMTP login of a
// pool. A cell never reads an unlabelled Secret for its mail setting.
const poolMailCredentialLabelSelector = "varroa.dev/pool-mail-credential=true"

// defaultPoolMailPort is the SMTP port a pool uses when it names none.
const defaultPoolMailPort = 587

// Reasons of the PoolMail condition.
const (
	ReasonPoolMailRendered            = "Rendered"
	ReasonPoolMailCredentialMissing   = "CredentialMissing"
	ReasonPoolMailCredentialMalformed = "CredentialMalformed"
	ReasonPoolMailPluginMissing       = "PluginMissing"
)

// poolMailSet is the rendered result of one pool's mail setting. Unclassified
// holds the unclassified keys of the mail setting. The mailer value carries
// the SMTP password, so only the desired-state command builder reads it.
// Reason and Faults feed the PoolMail condition.
type poolMailSet struct {
	Unclassified map[string]any
	Reason       string
	Faults       []string
}

// fail records one fault. The first fault sets the reason of the set.
func (s *poolMailSet) fail(reason, message string) {
	if s.Reason == ReasonPoolMailRendered || s.Reason == "" {
		s.Reason = reason
	}
	s.Faults = append(s.Faults, message)
}

// renderPoolMail turns the pool's mail setting into the unclassified keys
// of the cell's JCasC. A missing mailer plugin renders nothing. A login
// that is missing or malformed renders nothing. An email-ext plugin that
// is not pinned keeps the mailer and records a fault.
//
// The email-ext entry carries no credentialsId. The mailer login is the
// only SMTP login, because email-ext resolves a credentialsId as the run's
// own authentication, and a SYSTEM-scope credential is not visible there.
func renderPoolMail(m *v1alpha1.CellPoolMail, secrets []map[string][]byte, listErr error, coreSet []pluginlock.PluginEntry) poolMailSet {
	out := poolMailSet{Reason: ReasonPoolMailRendered}
	if m == nil {
		return out
	}
	if !pluginPinned(coreSet, "mailer") {
		out.fail(ReasonPoolMailPluginMissing, `plugin "mailer" is not in the pinned plugin set; no pool mail renders`)
		return out
	}

	var username, password string
	if m.CredentialRef != "" {
		if listErr != nil {
			out.fail(ReasonPoolMailCredentialMissing, fmt.Sprintf("list pool mail credential secrets: %v", listErr))
			return out
		}
		data := findPoolLibrarySecret(secrets, m.CredentialRef)
		if data == nil {
			out.fail(ReasonPoolMailCredentialMissing, fmt.Sprintf("no secret %q labelled %s in the operator namespace",
				m.CredentialRef, poolMailCredentialLabelSelector))
			return out
		}
		username = string(data["username"])
		password = string(data["password"])
		if username == "" || password == "" {
			out.fail(ReasonPoolMailCredentialMalformed, fmt.Sprintf("secret %q needs non-empty username and password data keys", m.CredentialRef))
			return out
		}
		if strings.Contains(username, unresolvedVarToken) || strings.Contains(password, unresolvedVarToken) {
			out.fail(ReasonPoolMailCredentialMalformed, fmt.Sprintf("secret %q carries an unresolved %s substitution token", m.CredentialRef, unresolvedVarToken))
			return out
		}
	}

	port := int(m.Port)
	if port == 0 {
		port = defaultPoolMailPort
	}
	useSsl, useTLS := false, false
	switch m.Security {
	case "tls":
		useSsl = true
	case "starttls", "":
		useTLS = true
	}

	mailer := map[string]any{
		"smtpHost": m.Host,
		"smtpPort": strconv.Itoa(port),
		"useSsl":   useSsl,
		"useTls":   useTLS,
		"charset":  "UTF-8",
	}
	if m.ReplyTo != "" {
		mailer["replyToAddress"] = m.ReplyTo
	}
	if m.DefaultSuffix != "" {
		mailer["defaultSuffix"] = m.DefaultSuffix
	}
	if m.CredentialRef != "" {
		mailer["authentication"] = map[string]any{"username": username, "password": password}
	}

	unclassified := map[string]any{
		"mailer":   mailer,
		"location": map[string]any{"adminAddress": m.From},
	}

	if !pluginPinned(coreSet, "email-ext") {
		out.fail(ReasonPoolMailPluginMissing, `plugin "email-ext" is not in the pinned plugin set; the mailer renders and emailext is absent`)
	} else {
		account := map[string]any{
			"smtpHost": m.Host,
			"smtpPort": strconv.Itoa(port),
			"useSsl":   useSsl,
			"useTls":   useTLS,
			"address":  m.From,
		}
		emailExt := map[string]any{
			"mailAccount": account,
			"charset":     "UTF-8",
		}
		if m.DefaultSuffix != "" {
			emailExt["defaultSuffix"] = m.DefaultSuffix
		}
		if m.ReplyTo != "" {
			emailExt["defaultReplyTo"] = m.ReplyTo
		}
		unclassified["email-ext"] = emailExt
	}

	out.Unclassified = unclassified
	return out
}

// injectPoolMail writes the pool's unclassified keys into the cell's
// desired-state JCasC. The pool wins over the bundle for each key it sets,
// except location: the pool sets only its sub-keys and the bundle keeps
// location.url. An empty keys map returns jcasc unchanged.
func injectPoolMail(jcasc string, keys map[string]any) (string, error) {
	if len(keys) == 0 {
		return jcasc, nil
	}
	var doc map[string]any
	if strings.TrimSpace(jcasc) != "" {
		if err := yaml.Unmarshal([]byte(jcasc), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for pool mail: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	unclassified, _ := doc["unclassified"].(map[string]any)
	if unclassified == nil {
		unclassified = map[string]any{}
		doc["unclassified"] = unclassified
	}
	for key, value := range keys {
		if key == "location" {
			sub, _ := value.(map[string]any)
			existing, _ := unclassified["location"].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
				unclassified["location"] = existing
			}
			for k, v := range sub {
				existing[k] = v
			}
			continue
		}
		unclassified[key] = value
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after pool mail injection: %w", err)
	}
	return string(out), nil
}

// applyPoolMail renders the pool's mail setting onto a cell's resolved
// bundle. It sets only mat.CellPoolMail, which the desired-state command
// builder reads. No ConfigMap writer reads it, so the SMTP password never
// reaches one. It is a no-op for a controller that is not a cell. It sets
// the PoolMail condition on cr, and it removes that condition when the pool
// declares no mail setting.
func (r *Reconciler) applyPoolMail(ctx context.Context, cr *v1alpha1.Controller, mat *bundle.MaterializedBundle, coreSet []pluginlock.PluginEntry) error {
	if !isCellController(cr) {
		return nil
	}

	// A read failure fails the resolve. It must not read as a pool that no
	// longer declares mail, because that would leave the cell on a stale
	// mailer without a warning.
	var mail *v1alpha1.CellPoolMail
	if name := cr.Labels[cells.LabelCellPool]; name != "" && r.store != nil {
		pool, err := crdstore.Get[v1alpha1.CellPool](ctx, r.store, name, "")
		switch {
		case err == nil:
			mail = pool.Spec.Mail
		case !apierrors.IsNotFound(err):
			return fmt.Errorf("read cell pool %q: %w", name, err)
		}
	}

	var secrets []map[string][]byte
	var listErr error
	if mail != nil && mail.CredentialRef != "" {
		secrets, listErr = r.client.ListSecrets(ctx, r.getOperatorNamespace(), poolMailCredentialLabelSelector)
	}

	set := renderPoolMail(mail, secrets, listErr, coreSet)
	mat.CellPoolMail = set.Unclassified

	if mail == nil {
		cr.Status.Conditions = removeConditionByType(cr.Status.Conditions, v1alpha1.ConditionPoolMail)
		return nil
	}
	cond := v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPoolMail,
		Status:  metav1.ConditionTrue,
		Reason:  set.Reason,
		Message: "pool mail rendered",
	}
	if len(set.Faults) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Message = strings.Join(set.Faults, "; ")
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, cond)
	return nil
}
