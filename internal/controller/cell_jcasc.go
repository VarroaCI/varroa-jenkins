package controller

import (
	"context"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bundle"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// cellJCascDocument is the fixed JCasC fragment every cell merges onto its
// composed jenkins.yaml (design D11): no local executors, since every build
// runs on a tenant cloud's agent, and the most durable pipeline flow so a
// cell loss never loses step state. The pipeline mesh replicates a
// running build's flow files, and a lower level writes them at a
// checkpoint instead of at each step, so a copied set would be stale by
// whole stages.
const cellJCascDocument = `jenkins:
  numExecutors: 0
unclassified:
  globalDefaultFlowDurabilityLevel:
    durabilityHint: MAX_SURVIVABILITY
`

// cellGitHostKeyDefault is the git host-key strategy a cell uses when its
// composed jenkins.yaml sets none. It is the merge base, so a strategy in
// the cell bundle wins. A tenant agent pod and the cell home start with no
// known_hosts file, so the git-client default strategy fails every SSH
// checkout.
const cellGitHostKeyDefault = `security:
  gitHostKeyVerificationConfiguration:
    sshHostKeyVerificationStrategy: "acceptFirstConnectionStrategy"
`

// The operator's own constant agent pod template (design D11). A
// JenkinsVersionProfile carries no pod template, so this mirrors the
// starter bundle's pinned agent image and resources
// (bundles/starter/jenkins.yaml).
const (
	tenantAgentJnlpImage                 = "jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21"
	tenantAgentJnlpResourceRequestCPU    = "100m"
	tenantAgentJnlpResourceRequestMemory = "256Mi"
	tenantAgentJnlpResourceLimitCPU      = "500m"
	tenantAgentJnlpResourceLimitMemory   = "512Mi"
	tenantAgentJnlpWorkingDir            = "/home/jenkins/agent"
)

// TenantAgentAdoptBound is the longest time from a cell JVM's last pod
// refresh to the moment a resume on another JVM adopts one of its agent
// nodes. The recorder can take the silent-cell window to see the cell as
// gone, and the resume then takes at most the arm window and its deadline.
// An agent pod that ran a node body must live this long, because a resume
// adopts that node and the pod connects to the new JVM again.
const TenantAgentAdoptBound = recordstore.SilentCellWindow + recordstore.ResumeAdoptBound

// tenantAgentGCTimeoutSeconds is the orphan-pod timeout each tenant cloud
// gives the kubernetes plugin. A cell home is an emptyDir, so a new cell JVM
// holds no node for an agent pod the old JVM launched until a resume adopts
// it. Every live JVM refreshes its own agent pods once a minute, and the
// plugin deletes a pod of its cloud that no JVM refreshed for this long.
// The value is TenantAgentAdoptBound plus a four-minute margin, so the
// plugin never deletes a pod that a resume can still adopt. The plugin
// cannot read the pod phase, so the operator deletes a stale pod that is
// not Running much sooner (tenantAgentPendingReapAfter).
const tenantAgentGCTimeoutSeconds = 2400

// tenantAgentEndpoint returns the in-cluster URL of one slot's agent
// Service. An agent pod outlives the Jenkins JVM that launched it, so its
// URL names an endpoint the operator can repoint rather than the cell's own
// Service.
func tenantAgentEndpoint(team, namespace string, slot int32) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", tenantAgentServiceName(team, slot), namespace)
}

// tenantCloudFor builds the one kubernetes cloud a placed tenant gets
// (design D11): usageRestricted, the operator's jnlp pod template first,
// then the pod templates lifted from the tenant's bundle in bundle order.
// Every template carries the tenant's ServiceAccount and the reserved node
// label the agent fence reads. The "<team>-" template-name prefix is
// readability only and carries no authorization meaning. agentEndpoint is
// the tenant's own agent Service, never the cell's own endpoint.
func tenantCloudFor(team, agentNamespace, agentEndpoint string, lifted []bundle.TenantCloudTemplate) bundle.TenantCloud {
	cloud := bundle.TenantCloud{
		Name:                     team,
		Namespace:                agentNamespace,
		JenkinsURL:               agentEndpoint,
		CredentialsID:            tenantCloudCredentialID(team),
		UsageRestricted:          true,
		WebSocket:                true,
		GarbageCollectionTimeout: tenantAgentGCTimeoutSeconds,
		Templates: []bundle.TenantCloudTemplate{{
			Name:           team + "-default",
			Namespace:      agentNamespace,
			ServiceAccount: tenantAgentServiceAccountName(team),
			Labels:         fmt.Sprintf("%s %s-default", tenantNodeLabel(team), team),
			Containers: []bundle.TenantCloudContainer{{
				Name:                  "jnlp",
				Image:                 tenantAgentJnlpImage,
				ResourceRequestCPU:    tenantAgentJnlpResourceRequestCPU,
				ResourceRequestMemory: tenantAgentJnlpResourceRequestMemory,
				ResourceLimitCPU:      tenantAgentJnlpResourceLimitCPU,
				ResourceLimitMemory:   tenantAgentJnlpResourceLimitMemory,
				WorkingDir:            tenantAgentJnlpWorkingDir,
			}},
		}},
	}
	cloud.Templates = append(cloud.Templates, lifted...)
	return cloud
}

// tenantNodeLabel returns the reserved node label design D12's agent fence
// reads. It carries the authorization meaning; the "<team>-" prefix on a
// pod template's other labels does not.
func tenantNodeLabel(team string) string {
	return "varroa-tenant-" + team
}

// tenantCloudCredential is bundle.CellCloudCredential's name inside this
// package: one SYSTEM-scope secret-text credential to inject for a tenant
// cloud (design D11).
type tenantCloudCredential = bundle.CellCloudCredential

// injectTenantCloudCredentials appends one SYSTEM-scope secretText
// credential per tenant cloud to jenkins.yaml's global credentials domain,
// replacing any existing entry with the same id first. It mirrors
// injectArtifactCredential: MergeJenkinsYAML replaces a list wholesale on
// conflict, so this list-aware edit runs on the already-parsed document
// instead.
//
// The caller MUST run this only on the JcascYaml sent to the mite, never on
// content that reaches a ConfigMap (design D8: "No secret value enters a
// ConfigMap"). buildDesiredStateCommand is the one caller.
func injectTenantCloudCredentials(yamlDoc string, creds []tenantCloudCredential) (string, error) {
	if len(creds) == 0 {
		return yamlDoc, nil
	}

	var doc map[string]any
	if strings.TrimSpace(yamlDoc) != "" {
		if err := yaml.Unmarshal([]byte(yamlDoc), &doc); err != nil {
			return "", fmt.Errorf("parse jcasc for tenant cloud credential injection: %w", err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}

	credentials, _ := doc["credentials"].(map[string]any)
	if credentials == nil {
		credentials = map[string]any{}
		doc["credentials"] = credentials
	}
	system, _ := credentials["system"].(map[string]any)
	if system == nil {
		system = map[string]any{}
		credentials["system"] = system
	}
	domainCredentials, _ := system["domainCredentials"].([]any)

	for _, c := range creds {
		for _, d := range domainCredentials {
			dm, ok := d.(map[string]any)
			if !ok {
				continue
			}
			existing, _ := dm["credentials"].([]any)
			dm["credentials"] = removeCredentialByID(existing, c.ID)
		}
	}

	var globalDomain map[string]any
	for _, d := range domainCredentials {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if _, hasDomain := dm["domain"]; !hasDomain {
			globalDomain = dm
			break
		}
	}
	if globalDomain == nil {
		globalDomain = map[string]any{"credentials": []any{}}
		domainCredentials = append(domainCredentials, globalDomain)
	}

	existing, _ := globalDomain["credentials"].([]any)
	for _, c := range creds {
		existing = append(existing, map[string]any{
			"string": map[string]any{
				"scope":  "SYSTEM",
				"id":     c.ID,
				"secret": c.Secret,
			},
		})
	}
	globalDomain["credentials"] = existing
	system["domainCredentials"] = domainCredentials

	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal jcasc after tenant cloud credential injection: %w", err)
	}
	return string(out), nil
}

// applyCellContent assembles a cell's tenant items and clouds onto mat, and
// merges them in (design D7, D10, D11). It records the BundleKeysIgnored,
// PodTemplateSkipped, and CredentialSkipped tenantWarnings on
// cr.Status.Cell. A no-op for a controller that is not a cell.
//
// It does NOT merge each tenant's pipeline-template fragment into
// mat.JenkinsYAML. It only carries them on mat.CellTemplateFragments, for
// resolveBundleForController to merge in later (design D7, tenant-isolation).
//
// It does NOT inject the SYSTEM-scope tenant-cloud credentials (design
// D8): mat.JenkinsYAML is written into the CASC ConfigMap
// (resolveBundleForController's caller), and no secret value may enter a
// ConfigMap. The credentials are carried on mat.CellCloudCredentials
// instead, unread by the ConfigMap write path, for
// buildDesiredStateCommand to inject into the mite-bound JcascYaml alone.
func (r *Reconciler) applyCellContent(ctx context.Context, cr *v1alpha1.Controller, mat *bundle.MaterializedBundle, controllerEndpoint string) error {
	if !isCellController(cr) {
		return nil
	}

	withDefault, err := bundle.MergeJenkinsYAML(cellGitHostKeyDefault, mat.JenkinsYAML)
	if err != nil {
		return fmt.Errorf("merge cell git host-key default: %w", err)
	}
	mat.JenkinsYAML = withDefault

	merged, err := bundle.MergeJenkinsYAML(mat.JenkinsYAML, cellJCascDocument)
	if err != nil {
		return fmt.Errorf("merge cell jcasc document: %w", err)
	}
	mat.JenkinsYAML = merged

	teams := r.listPlacedTeams(ctx, cr)
	state, err := r.assembleCellContent(ctx, cr, teams, controllerEndpoint)
	if err != nil {
		return err
	}
	mat.ItemsYAML = state.ItemsYAML

	withClouds, err := bundle.InjectTenantClouds(mat.JenkinsYAML, state.Clouds)
	if err != nil {
		return fmt.Errorf("inject tenant clouds: %w", err)
	}
	mat.JenkinsYAML = withClouds
	mat.CellCloudCredentials = state.CloudCredentials

	// Carried, not injected: a tenant's template body must not reach
	// ResolveVars, which runs on mat.JenkinsYAML right after this function
	// returns. resolveBundleForController merges each fragment in once
	// ResolveVars and the unresolved-variable check both complete.
	mat.CellTemplateFragments = state.TemplateFragments

	recordCellTenantWarnings(cr, state.Warnings)
	return nil
}

// tenantSlotOnCell answers the slot the tenant holds on cr. A standby, a
// tenant with no member list, and a tenant no member of which names this
// cell all answer slot 0.
//
// The answer is the slot whose agent Service this cell's tenant cloud
// dials, so an agent launched here reaches an endpoint that follows the
// slot's own holder.
func tenantSlotOnCell(team *v1alpha1.Team, cr *v1alpha1.Controller) int32 {
	if team.Status.Placement == nil {
		return tenantSlotZero
	}
	for _, m := range team.Status.Placement.Members {
		if m.Cell != cr.Name || m.CellNamespace != cr.Namespace {
			continue
		}
		if m.Role == v1alpha1.TeamMemberRoleStandby || m.Slot == nil {
			return tenantSlotZero
		}
		return *m.Slot
	}
	return tenantSlotZero
}
