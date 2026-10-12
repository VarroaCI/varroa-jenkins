package controller

import (
	"context"
	"fmt"
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

// poolLibraryCredentialLabelSelector marks a Secret as the git login of a
// pool library. A cell never reads an unlabelled Secret for a library.
const poolLibraryCredentialLabelSelector = "varroa.dev/pool-library-credential=true"

// unresolvedVarToken marks a JCasC substitution. A pool value that carries
// it would be interpolated by JCasC, so the operator refuses it.
const unresolvedVarToken = "${"

// poolLibraryCredentialIDPrefix prefixes the Jenkins credential ID of each
// pool library. The ID is the prefix plus the library name.
const poolLibraryCredentialIDPrefix = "varroa-pool-library-"

// Reasons of the PoolLibraries condition.
const (
	ReasonPoolLibrariesRendered          = "Rendered"
	ReasonPoolLibraryCredentialMissing   = "CredentialMissing"
	ReasonPoolLibraryCredentialMalformed = "CredentialMalformed"
	ReasonPoolLibraryInvalid             = "LibraryInvalid"
	ReasonPoolLibraryPluginMissing       = "PluginMissing"
)

// poolLibraryPlugins lists the plugins a pool library needs. A cell whose
// plugin set omits one of them renders no pool library.
var poolLibraryPlugins = []string{"pipeline-groovy-lib", "git"}

// poolLibrarySet is the rendered result of one cell's pool libraries.
// Entries go into unclassified.globalLibraries.libraries. Credentials go
// into the mite command only, never into a ConfigMap. Reason and Faults
// feed the PoolLibraries condition.
type poolLibrarySet struct {
	Entries     []any
	Credentials []any
	Reason      string
	Faults      []string
}

// poolLibraryFault is one library held from rendering.
type poolLibraryFault struct {
	reason  string
	message string
}

// renderPoolLibraries turns the pool's declared libraries into JCasC
// entries and SYSTEM credentials. A library that fails validation is held
// and the other libraries still render. When a required plugin is not
// pinned, no library renders. secrets is the labelled Secret list and
// listErr its read error; both are used only when a library names a
// credential.
func renderPoolLibraries(libs []v1alpha1.CellPoolLibrary, secrets []map[string][]byte, listErr error, coreSet []pluginlock.PluginEntry) poolLibrarySet {
	out := poolLibrarySet{Reason: ReasonPoolLibrariesRendered}
	if len(libs) == 0 {
		return out
	}

	var faults []poolLibraryFault
	for _, plugin := range poolLibraryPlugins {
		if !pluginPinned(coreSet, plugin) {
			faults = append(faults, poolLibraryFault{
				reason:  ReasonPoolLibraryPluginMissing,
				message: fmt.Sprintf("plugin %q is not in the pinned plugin set; no pool library renders", plugin),
			})
			out.setFaults(faults)
			return out
		}
	}

	for _, lib := range libs {
		if strings.Contains(lib.Remote, unresolvedVarToken) || strings.Contains(lib.DefaultVersion, unresolvedVarToken) {
			faults = append(faults, poolLibraryFault{
				reason:  ReasonPoolLibraryInvalid,
				message: fmt.Sprintf("library %q carries a %s variable token in remote or defaultVersion", lib.Name, unresolvedVarToken),
			})
			continue
		}

		scm := map[string]any{"remote": lib.Remote}
		if lib.CredentialRef != "" {
			if listErr != nil {
				faults = append(faults, poolLibraryFault{
					reason:  ReasonPoolLibraryCredentialMissing,
					message: fmt.Sprintf("library %q: list pool-library credential secrets: %v", lib.Name, listErr),
				})
				continue
			}
			data := findPoolLibrarySecret(secrets, lib.CredentialRef)
			if data == nil {
				faults = append(faults, poolLibraryFault{
					reason: ReasonPoolLibraryCredentialMissing,
					message: fmt.Sprintf("library %q: no secret %q labelled %s in the operator namespace",
						lib.Name, lib.CredentialRef, poolLibraryCredentialLabelSelector),
				})
				continue
			}
			cred, fault := poolLibraryCredential(lib, data)
			if fault != nil {
				faults = append(faults, *fault)
				continue
			}
			out.Credentials = append(out.Credentials, cred)
			scm["credentialsId"] = poolLibraryCredentialIDPrefix + lib.Name
		}

		out.Entries = append(out.Entries, map[string]any{
			"name":                 lib.Name,
			"defaultVersion":       lib.DefaultVersion,
			"implicit":             lib.Implicit,
			"allowVersionOverride": lib.AllowVersionOverride,
			"retriever": map[string]any{
				"modernSCM": map[string]any{
					"scm": map[string]any{"git": scm},
				},
			},
		})
	}

	out.setFaults(faults)
	return out
}

// setFaults records the faults on the set. The reason of the set is the
// reason of the first fault.
func (s *poolLibrarySet) setFaults(faults []poolLibraryFault) {
	for i, f := range faults {
		if i == 0 {
			s.Reason = f.reason
		}
		s.Faults = append(s.Faults, f.message)
	}
}

// findPoolLibrarySecret returns the labelled Secret named name, or nil.
func findPoolLibrarySecret(secrets []map[string][]byte, name string) map[string][]byte {
	for _, rec := range secrets {
		if string(rec["_name"]) == name {
			return rec
		}
	}
	return nil
}

// poolLibraryCredential builds the Jenkins credential of one library from
// its Secret data. It accepts a username with exactly one of password or
// privateKey. An ssh remote needs privateKey. A passphrase goes only with
// privateKey. A value that carries a variable token is malformed.
func poolLibraryCredential(lib v1alpha1.CellPoolLibrary, data map[string][]byte) (any, *poolLibraryFault) {
	malformed := func(detail string) (any, *poolLibraryFault) {
		return nil, &poolLibraryFault{
			reason:  ReasonPoolLibraryCredentialMalformed,
			message: fmt.Sprintf("library %q: secret %q %s", lib.Name, lib.CredentialRef, detail),
		}
	}

	username := string(data["username"])
	password := string(data["password"])
	privateKey := string(data["privateKey"])
	passphrase := string(data["passphrase"])
	_, hasPassphrase := data["passphrase"]

	if username == "" {
		return malformed("has no username data key")
	}
	for _, v := range []string{username, password, privateKey, passphrase} {
		if strings.Contains(v, unresolvedVarToken) {
			return malformed("carries an unresolved " + unresolvedVarToken + " substitution token")
		}
	}

	hasPassword := password != ""
	hasKey := privateKey != ""
	switch {
	case hasPassword && hasKey:
		return malformed("carries both password and privateKey; set exactly one")
	case !hasPassword && !hasKey:
		return malformed("carries neither password nor privateKey")
	case !strings.HasPrefix(lib.Remote, "https://"):
		if !hasKey {
			return malformed("is for an ssh remote and needs a privateKey data key")
		}
	}
	if hasPassword && hasPassphrase {
		return malformed("carries a passphrase, which is valid only with privateKey")
	}

	id := poolLibraryCredentialIDPrefix + lib.Name
	if hasPassword {
		return map[string]any{"usernamePassword": map[string]any{
			"scope":    "SYSTEM",
			"id":       id,
			"username": username,
			"password": password,
		}}, nil
	}

	entry := map[string]any{
		"scope":    "SYSTEM",
		"id":       id,
		"username": username,
		"privateKeySource": map[string]any{
			"directEntry": map[string]any{"privateKey": privateKey},
		},
	}
	if hasPassphrase {
		entry["passphrase"] = passphrase
	}
	return map[string]any{"basicSSHUserPrivateKey": entry}, nil
}

// injectPoolLibraries sets unclassified.globalLibraries.libraries on the
// cell's jenkins.yaml. It drops each bundle entry whose name a pool entry
// reuses, then appends the pool entries in spec order. It always writes
// the key, as an empty list when no entry remains. An empty list keeps a
// removed library from staying live in Jenkins after a reload.
func injectPoolLibraries(jenkinsYAML string, entries []any) (string, error) {
	var doc map[string]any
	if strings.TrimSpace(jenkinsYAML) != "" {
		if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for pool libraries: %w", err)
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
	globalLibs, _ := unclassified["globalLibraries"].(map[string]any)
	if globalLibs == nil {
		globalLibs = map[string]any{}
		unclassified["globalLibraries"] = globalLibs
	}

	poolNames := make(map[string]bool, len(entries))
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok {
			if name, ok := m["name"].(string); ok {
				poolNames[name] = true
			}
		}
	}

	kept := []any{}
	existing, _ := globalLibs["libraries"].([]any)
	for _, e := range existing {
		m, ok := e.(map[string]any)
		if ok {
			if name, _ := m["name"].(string); poolNames[name] {
				continue
			}
		}
		kept = append(kept, e)
	}
	globalLibs["libraries"] = append(kept, entries...)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after pool library injection: %w", err)
	}
	return string(out), nil
}

// applyPoolLibraries renders the pool's trusted libraries onto a cell's
// resolved bundle. It runs after the tenant template loop and after
// ResolveVars, so no pool value meets the catalog variable pass. It is a
// no-op for a controller that is not a cell. It sets the PoolLibraries
// condition on cr, and it removes that condition when the pool declares no
// library.
func (r *Reconciler) applyPoolLibraries(ctx context.Context, cr *v1alpha1.Controller, mat *bundle.MaterializedBundle, coreSet []pluginlock.PluginEntry) error {
	if !isCellController(cr) {
		return nil
	}

	// A read failure fails the resolve. It must not read as a pool that no
	// longer declares libraries, because that would unload them from Jenkins.
	var libs []v1alpha1.CellPoolLibrary
	if name := cr.Labels[cells.LabelCellPool]; name != "" && r.store != nil {
		pool, err := crdstore.Get[v1alpha1.CellPool](ctx, r.store, name, "")
		switch {
		case err == nil:
			libs = pool.Spec.Libraries
		case !apierrors.IsNotFound(err):
			return fmt.Errorf("read cell pool %q: %w", name, err)
		}
	}

	var secrets []map[string][]byte
	var listErr error
	for _, lib := range libs {
		if lib.CredentialRef != "" {
			secrets, listErr = r.client.ListSecrets(ctx, r.getOperatorNamespace(), poolLibraryCredentialLabelSelector)
			break
		}
	}

	set := renderPoolLibraries(libs, secrets, listErr, coreSet)

	entries := set.Entries
	if entries == nil {
		entries = []any{}
	}
	withLibs, err := injectPoolLibraries(mat.JenkinsYAML, entries)
	if err != nil {
		return err
	}
	mat.JenkinsYAML = withLibs
	mat.CellPoolLibraryCredentials = set.Credentials

	if len(libs) == 0 {
		cr.Status.Conditions = removeConditionByType(cr.Status.Conditions, v1alpha1.ConditionPoolLibraries)
		return nil
	}
	cond := v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPoolLibraries,
		Status:  metav1.ConditionTrue,
		Reason:  set.Reason,
		Message: "all pool libraries rendered",
	}
	if len(set.Faults) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Message = strings.Join(set.Faults, "; ")
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, cond)
	return nil
}
