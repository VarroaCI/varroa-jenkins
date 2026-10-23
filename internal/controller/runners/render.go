package runners

import (
	"strings"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// defaultPort is the ssh launcher port an unset spec.port renders as. The
// CRD carries no default: a default would make the field present on an
// inbound runner, which the inbound CEL rule forbids.
const defaultPort = 22

// reservedTenantLabel is the fence's only attribution signal (design D4):
// the token varroa-tenant-<tenant>, read as an exact whole-token match. Every
// rendered node carries it first.
func reservedTenantLabel(tenant string) string {
	return "varroa-tenant-" + tenant
}

// renderNode builds one jenkins.nodes entry for a delivered runner, per
// design D3. The caller filters to delivered runners: this function renders
// whatever it is given.
func renderNode(runner *v1alpha1.Runner) map[string]any {
	tenant := runner.Spec.Tenant

	labels := make([]string, 0, 2+len(runner.Spec.Labels))
	labels = append(labels, reservedTenantLabel(tenant), tenant+"-runner")
	for _, l := range runner.Spec.Labels {
		labels = append(labels, tenant+"-"+l)
	}

	numExecutors := runner.Spec.NumExecutors
	if numExecutors == 0 {
		numExecutors = 1
	}

	permanent := map[string]any{
		"name":              tenant + "-" + runner.Name,
		"labelString":       strings.Join(labels, " "),
		"remoteFS":          runner.Spec.RemoteFS,
		"numExecutors":      numExecutors,
		"mode":              "EXCLUSIVE",
		"retentionStrategy": "always",
		"launcher":          renderLauncher(runner),
	}
	return map[string]any{"permanent": permanent}
}

// renderLauncher builds the launcher sub-block: ssh with host, port,
// credentialsId, and an optional javaPath, or inbound with webSocket true.
func renderLauncher(runner *v1alpha1.Runner) map[string]any {
	if runner.Spec.Launcher == launcherInbound {
		return map[string]any{
			"inbound": map[string]any{"webSocket": true},
		}
	}

	port := defaultPort
	if runner.Spec.Port != nil {
		port = *runner.Spec.Port
	}
	ssh := map[string]any{
		"host":          runner.Spec.Host,
		"port":          port,
		"credentialsId": credentialID(runner.Name),
		// A cell's home is an emptyDir, so no known_hosts file can outlive
		// a roll; the launcher trusts the host key it sees first and the
		// operator's per-runner egress rule pins the address (ruling (kk)).
		"sshHostKeyVerificationStrategy": map[string]any{
			"manuallyTrustedKeyVerificationStrategy": map[string]any{
				"requireInitialManualTrust": false,
			},
		},
	}
	if runner.Spec.JavaPath != "" {
		ssh["javaPath"] = runner.Spec.JavaPath
	}
	return map[string]any{"ssh": ssh}
}

// credentialID names the SYSTEM-scope credential an ssh runner's node
// launcher references (design D5). It is built from the Runner object name,
// never the rendered node name, because a Runner is cluster-scoped and
// already unique fleet-wide.
func credentialID(runnerName string) string {
	return "varroa-runner-" + runnerName
}
