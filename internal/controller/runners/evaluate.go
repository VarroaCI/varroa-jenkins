package runners

import (
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// The Ready=False reasons this package produces. TenantNameUnsupported and
// RunnerNameUnsupported are distinct reasons: each names the object an
// operator must rename to make the rendered node name <tenant>-<runner name>
// legal.
const (
	ReasonTenantNotFound        = "TenantNotFound"
	ReasonTenantNotPipelines    = "TenantNotPipelines"
	ReasonLauncherUnsupported   = "LauncherUnsupported"
	ReasonTenantNameUnsupported = "TenantNameUnsupported"
	ReasonRunnerNameUnsupported = "RunnerNameUnsupported"
	ReasonAwaitingPlacement     = "AwaitingPlacement"
	ReasonOffline               = "Offline"
	ReasonConnected             = "Connected"
)

// ConditionTypeReady is the one condition type a Runner's status carries.
const ConditionTypeReady = "Ready"

const (
	launcherSSH     = "ssh"
	launcherInbound = "inbound"
)

// evaluation is the result of applying tick steps 3 to 6 of
// specs/external-runners/spec.md to one Runner: the derived node name, the
// cell it belongs on (empty when unplaced), and whether it is delivered.
// evaluateRunner performs no I/O and raises no fault from step 7 (the
// credential read): a caller that also needs step 7 layers that fault in
// afterward, turning Delivered false and replacing Reason/Message when the
// credential check finds a fault.
type evaluation struct {
	NodeName      string
	Cell          string
	CellNamespace string
	Cluster       string
	Delivered     bool
	Reason        string
	Message       string
}

// evaluateRunner applies tick steps 3 to 6. tenant is nil when spec.tenant names
// no Tenant object. Both the ticker (Tick) and the desired-state accessor
// (BuildContribution) call this so the reported status and the rendered
// node set can never drift apart.
func evaluateRunner(runner *v1alpha1.Runner, tenant *v1alpha1.Tenant) evaluation {
	eval := evaluation{
		NodeName: runner.Spec.Tenant + "-" + runner.Name,
	}

	if tenant != nil && tenant.Status.Placement != nil {
		eval.Cell = tenant.Status.Placement.Cell
		eval.CellNamespace = tenant.Status.Placement.CellNamespace
		eval.Cluster = tenant.Status.Placement.Cluster
	}

	switch {
	case tenant == nil:
		eval.Reason = ReasonTenantNotFound
		eval.Message = "spec.tenant names no Tenant"
	case tenant.Spec.Pipelines == nil:
		eval.Reason = ReasonTenantNotPipelines
		eval.Message = "the named Tenant sets no spec.pipelines"
	case runner.Spec.Launcher != launcherSSH && runner.Spec.Launcher != launcherInbound:
		eval.Reason = ReasonLauncherUnsupported
		eval.Message = "spec.launcher is neither ssh nor inbound"
	case !isDNS1123Label(runner.Spec.Tenant):
		eval.Reason = ReasonTenantNameUnsupported
		eval.Message = "the Tenant name is not a DNS-1123 label"
	case !isDNS1123Label(runner.Name):
		eval.Reason = ReasonRunnerNameUnsupported
		eval.Message = "the Runner name is not a DNS-1123 label"
	case eval.Cell == "" || eval.CellNamespace == "":
		eval.Reason = ReasonAwaitingPlacement
		eval.Message = "the Tenant has no cell placement"
	}

	eval.Delivered = eval.Reason == ""
	return eval
}

// isDNS1123Label reports whether name satisfies the stricter DNS-1123
// label rule a Kubernetes object name's own DNS-1123 subdomain rule does
// not enforce (a subdomain may carry a dot a label cannot).
func isDNS1123Label(name string) bool {
	return len(validation.IsDNS1123Label(name)) == 0
}
