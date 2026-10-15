package controller

import (
	"context"
	"fmt"
	"net/url"
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

// Reasons of the PoolGiteaServers condition.
const (
	ReasonPoolGiteaServersRendered     = "Rendered"
	ReasonPoolGiteaServerPluginMissing = "PluginMissing"
	ReasonPoolGiteaServerInvalid       = "ServerInvalid"
)

// poolGiteaServerSet is the rendered result of one pool's Gitea servers.
// Entries go into unclassified.giteaServers.servers. The set carries no
// credential, because a cell never needs one for a server entry. Reason and
// Faults feed the PoolGiteaServers condition.
type poolGiteaServerSet struct {
	Entries []any
	Reason  string
	Faults  []string
}

// fail records one fault. The first fault sets the reason of the set.
func (s *poolGiteaServerSet) fail(reason, message string) {
	if s.Reason == ReasonPoolGiteaServersRendered || s.Reason == "" {
		s.Reason = reason
	}
	s.Faults = append(s.Faults, message)
}

// renderPoolGiteaServers turns the pool's Gitea servers into server entries.
// Each entry sets manageHooks to false and carries no credential. The
// operator never grants webhook management to a cell. A server that carries
// a variable token is held and the others still render. When the gitea
// plugin is not pinned, no server renders.
func renderPoolGiteaServers(servers []v1alpha1.CellPoolGiteaServer, coreSet []pluginlock.PluginEntry) poolGiteaServerSet {
	out := poolGiteaServerSet{Reason: ReasonPoolGiteaServersRendered}
	if len(servers) == 0 {
		return out
	}
	if !pluginPinned(coreSet, giteaPlugin) {
		out.fail(ReasonPoolGiteaServerPluginMissing, fmt.Sprintf("plugin %q is not in the pinned plugin set; no pool Gitea server renders", giteaPlugin))
		return out
	}

	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		if field := giteaServerTokenField(s); field != "" {
			out.fail(ReasonPoolGiteaServerInvalid, fmt.Sprintf("Gitea server %q: field %s carries a %s variable token", s.ServerURL, field, unresolvedVarToken))
			continue
		}

		// The list map key compares bytes, so two spellings of one forge pass
		// admission. The first spelling in spec order renders; the rest are
		// skipped.
		key := normalizeGiteaServerURL(s.ServerURL)
		if seen[key] {
			continue
		}
		seen[key] = true

		entry := map[string]any{
			"serverUrl":   strings.TrimRight(s.ServerURL, "/"),
			"manageHooks": false,
		}
		if s.DisplayName != "" {
			entry["displayName"] = s.DisplayName
		}
		if s.AliasURL != "" {
			entry["aliasUrl"] = strings.TrimRight(s.AliasURL, "/")
		}
		out.Entries = append(out.Entries, entry)
	}
	return out
}

// giteaServerTokenField returns the first field of a pool Gitea server that
// carries a variable token, or an empty string when no field does.
func giteaServerTokenField(s v1alpha1.CellPoolGiteaServer) string {
	switch {
	case strings.Contains(s.ServerURL, unresolvedVarToken):
		return "serverUrl"
	case strings.Contains(s.DisplayName, unresolvedVarToken):
		return "displayName"
	case strings.Contains(s.AliasURL, unresolvedVarToken):
		return "aliasUrl"
	}
	return ""
}

// normalizeGiteaServerURL returns the comparison form of a server URL. It
// matches the plugin's own server lookup. The scheme and host fold to lower
// case. A default port (80 for http, 443 for https) is dropped. The path
// keeps its case, because a path is case-sensitive. One trailing slash is
// removed. A value that does not parse keeps its text, without the slash.
func normalizeGiteaServerURL(u string) string {
	s := strings.TrimSpace(u)
	parsed, err := url.Parse(s)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return strings.TrimSuffix(s, "/")
	}

	scheme := strings.ToLower(parsed.Scheme)
	host := parsed.Host
	port := parsed.Port()
	if port != "" {
		host = host[:len(host)-len(port)-1]
	}
	host = strings.ToLower(host)
	isDefaultPort := (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
	if port != "" && !isDefaultPort {
		host += ":" + port
	}

	parsed.Scheme = scheme
	parsed.Host = host
	return strings.TrimSuffix(parsed.String(), "/")
}

// injectPoolGiteaServers sets unclassified.giteaServers.servers on the
// cell's jenkins.yaml. It drops each bundle entry whose server URL a pool
// entry reuses, then appends the pool entries in spec order. It always
// writes the key, as an empty list when no entry remains. An empty list
// unloads a removed server after a reload.
func injectPoolGiteaServers(jenkinsYAML string, entries []any) (string, error) {
	var doc map[string]any
	if strings.TrimSpace(jenkinsYAML) != "" {
		if err := yaml.Unmarshal([]byte(jenkinsYAML), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for pool gitea servers: %w", err)
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
	gitea, _ := unclassified["giteaServers"].(map[string]any)
	if gitea == nil {
		gitea = map[string]any{}
		unclassified["giteaServers"] = gitea
	}

	poolURLs := make(map[string]bool, len(entries))
	for _, e := range entries {
		if m, ok := e.(map[string]any); ok {
			if u, ok := m["serverUrl"].(string); ok {
				poolURLs[normalizeGiteaServerURL(u)] = true
			}
		}
	}

	kept := []any{}
	existing, _ := gitea["servers"].([]any)
	for _, e := range existing {
		m, ok := e.(map[string]any)
		if ok {
			if u, _ := m["serverUrl"].(string); poolURLs[normalizeGiteaServerURL(u)] {
				continue
			}
		}
		kept = append(kept, e)
	}
	gitea["servers"] = append(kept, entries...)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after pool gitea server injection: %w", err)
	}
	return string(out), nil
}

// applyPoolGiteaServers renders the pool's Gitea and Forgejo servers onto a
// cell's resolved bundle. It runs after ResolveVars, so no pool value meets
// the catalog variable pass. It is a no-op for a controller that is not a
// cell. When the gitea plugin is not pinned, the JCasC stays unchanged,
// because an unknown key fails the reload. It sets the PoolGiteaServers
// condition on cr, and it removes that condition when the pool declares no
// server.
func (r *Reconciler) applyPoolGiteaServers(ctx context.Context, cr *v1alpha1.Controller, mat *bundle.MaterializedBundle, coreSet []pluginlock.PluginEntry) error {
	if !isCellController(cr) {
		return nil
	}

	// A read failure fails the resolve. It must not read as a pool that no
	// longer declares servers, because that would unload them from Jenkins.
	var servers []v1alpha1.CellPoolGiteaServer
	if name := cr.Labels[cells.LabelCellPool]; name != "" && r.store != nil {
		pool, err := crdstore.Get[v1alpha1.CellPool](ctx, r.store, name, "")
		switch {
		case err == nil:
			servers = pool.Spec.GiteaServers
		case !apierrors.IsNotFound(err):
			return fmt.Errorf("read cell pool %q: %w", name, err)
		}
	}

	set := renderPoolGiteaServers(servers, coreSet)
	if pluginPinned(coreSet, giteaPlugin) {
		entries := set.Entries
		if entries == nil {
			entries = []any{}
		}
		withServers, err := injectPoolGiteaServers(mat.JenkinsYAML, entries)
		if err != nil {
			return err
		}
		mat.JenkinsYAML = withServers
	}

	if len(servers) == 0 {
		cr.Status.Conditions = removeConditionByType(cr.Status.Conditions, v1alpha1.ConditionPoolGiteaServers)
		return nil
	}
	cond := v1alpha1.ControllerCondition{
		Type:    v1alpha1.ConditionPoolGiteaServers,
		Status:  metav1.ConditionTrue,
		Reason:  set.Reason,
		Message: "all pool gitea servers rendered",
	}
	if len(set.Faults) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Message = strings.Join(set.Faults, "; ")
	}
	cr.Status.Conditions = setCondition(cr.Status.Conditions, cond)
	return nil
}
