package rbac

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// Generator produces JCasC authorizationStrategy YAML from the resolver.
type Generator struct {
	resolver *Resolver
	backend  Backend
	Logger   *slog.Logger
}

// NewGenerator creates a new Generator backed by the given resolver.
// If backend is nil, the default RoleStrategyBackend is used.
func NewGenerator(resolver *Resolver) *Generator {
	return &Generator{
		resolver: resolver,
		backend:  &RoleStrategyBackend{},
	}
}

// WithBackend sets a custom Backend for testing.
func (g *Generator) WithBackend(backend Backend) *Generator {
	g.backend = backend
	return g
}

// RoleStrategyBackend renders RoleAssignments into the role-strategy
// bucketed YAML format (global/items/agents).
type RoleStrategyBackend struct{}

// permissionGroupTitles maps a Jenkins permission's owner class (the part of
// the permission ID before the final segment) to the role-strategy UI group
// title. role-strategy's JCasC configurator resolves permissions via
// PermissionHelper.fromStrings(perms, /*allowPermissionId=*/false), which only
// accepts the UI format "GroupTitle/Name" — NOT the dotted Java ID
// "hudson.model.Hudson.Administer". Unrecognized IDs are silently dropped
// ("Ignoring unresolved permission"), so we must emit the UI form.
var permissionGroupTitles = map[string]string{
	"hudson.model.Hudson":   "Overall",
	"hudson.model.Computer": "Agent",
	"hudson.model.Item":     "Job",
	"hudson.model.Run":      "Run",
	"hudson.model.View":     "View",
	"hudson.scm.SCM":        "SCM",
	"com.cloudbees.plugins.credentials.CredentialsProvider": "Credentials",
}

// toRoleStrategyPermission converts a dotted Jenkins permission ID
// (e.g. "hudson.model.Item.Read") into the role-strategy UI format
// ("Job/Read"). Already-UI-format strings (containing "/") and unknown owner
// classes are returned unchanged.
func toRoleStrategyPermission(id string) string {
	if strings.Contains(id, "/") {
		return id
	}
	idx := strings.LastIndex(id, ".")
	if idx < 0 {
		return id
	}
	owner, name := id[:idx], id[idx+1:]
	if title, ok := permissionGroupTitles[owner]; ok {
		return title + "/" + name
	}
	return id
}

// rsRole is a YAML-serializable role entry for role-strategy.
type rsRole struct {
	Name        string              `yaml:"name"`
	Permissions []string            `yaml:"permissions"`
	Pattern     string              `yaml:"pattern,omitempty"`
	Entries     []map[string]string `yaml:"entries"`
}

// Render implements Backend by bucketing assignments into
// global, items, and agents maps.
func (b *RoleStrategyBackend) Render(assignments []RoleAssignment) (string, error) {
	buckets := map[string][]rsRole{
		"global": {},
		"items":  {},
		"agents": {},
	}

	for _, a := range assignments {
		if len(a.Permissions) == 0 {
			continue
		}
		bucket := "global"
		switch a.RoleType {
		case "Item":
			bucket = "items"
		case "Agent":
			bucket = "agents"
		}
		var entries []map[string]string
		for _, s := range a.Subjects {
			if s.Kind == "Group" {
				entries = append(entries, map[string]string{"group": s.Name})
			} else {
				entries = append(entries, map[string]string{"user": s.Name})
			}
		}
		perms := make([]string, len(a.Permissions))
		for i, p := range a.Permissions {
			perms[i] = toRoleStrategyPermission(p)
		}
		r := rsRole{
			Name:        a.RoleName,
			Permissions: perms,
			Entries:     entries,
		}
		if bucket != "global" {
			r.Pattern = a.Pattern
		}
		buckets[bucket] = append(buckets[bucket], r)
	}

	// Wrap under the top-level "jenkins" key so the document is a valid JCasC
	// configuration. RBAC is applied through the configuration-as-code apply
	// endpoint, which rejects a bare "authorizationStrategy" root with
	// "No configurator for the following root elements:authorizationStrategy".
	strategy := map[string]any{
		"jenkins": map[string]any{
			"authorizationStrategy": map[string]any{
				"roleBased": map[string]any{
					"roles": buckets,
				},
			},
		},
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(strategy); err != nil {
		return "", fmt.Errorf("encode rbac yaml: %w", err)
	}
	return buf.String(), nil
}

// Generate returns the JCasC authorizationStrategy.roleBased YAML block
// for the given controller. It resolves all role assignments (JenkinsRoleBindings,
// VarroaRole.JenkinsRoleRef, and legacy JenkinsPermissions) and renders them
// through the configured Backend.
func (g *Generator) Generate(controller *v1alpha1.Controller) (string, error) {
	yaml, _, err := g.GenerateWithAdminCheck(controller)
	return yaml, err
}

// GenerateWithAdminCheck is like Generate but also returns whether the
// generated assignments include at least one human/group administrator
// other than the synthesized mite role.
func (g *Generator) GenerateWithAdminCheck(c *v1alpha1.Controller) (yaml string, humanAdmin bool, err error) {
	if g.Logger != nil {
		g.Logger.Debug("generating rbac", "controller", c.Name, "namespace", c.Namespace)
	}
	assignments, err := g.resolver.JenkinsRoleAssignments(c)
	if err != nil {
		return "", false, fmt.Errorf("rbac generate: %w", err)
	}
	rendered, err := g.backend.Render(assignments)
	return rendered, HasHumanAdmin(assignments), err
}

// HasHumanAdmin reports whether any assignment grants global Administer to a
// human/group subject other than the synthesized mite role.
func HasHumanAdmin(assignments []RoleAssignment) bool {
	for _, a := range assignments {
		if a.RoleType != "Global" {
			continue
		}
		if !containsAdminister(a.Permissions) {
			continue
		}
		for _, s := range a.Subjects {
			if (s.Kind == "User" || s.Kind == "Group") && s.Name != "ROLE:varroa:system-mite" && s.Name != "ROLE:varroa:system-operator" {
				return true
			}
		}
	}
	return false
}

var administerPermissions = []string{
	"hudson.model.Hudson.Administer",
	"Overall.Administer",
	"Overall/Administer",
}

func containsAdminister(perms []string) bool {
	for _, p := range perms {
		if slices.Contains(administerPermissions, p) {
			return true
		}
	}
	return false
}
