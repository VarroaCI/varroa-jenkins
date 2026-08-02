package preflight

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

func checkQuota(ctx context.Context, deps Deps, draft *v1alpha1.Controller, defaults *v1alpha1.ProvisioningDefaults) Check {
	quotas, err := deps.ListResourceQuotas(ctx, draft.Namespace)
	if err != nil {
		return Check{
			ID:      "quota",
			Status:  "warn",
			Message: "unable to list ResourceQuotas: " + err.Error(),
		}
	}

	if len(quotas) == 0 {
		return Check{ID: "quota", Status: "pass", Message: "no ResourceQuota in namespace; headroom unknown"}
	}

	cpuReq := "1"
	memReq := "2Gi"
	stgReq := "10Gi"
	if draft.Spec.Resources != nil && draft.Spec.Resources.Requests != nil {
		if cpu, ok := draft.Spec.Resources.Requests[corev1.ResourceCPU]; ok && cpu.Value() > 0 {
			cpuReq = cpu.String()
		}
		if mem, ok := draft.Spec.Resources.Requests[corev1.ResourceMemory]; ok && mem.Value() > 0 {
			memReq = mem.String()
		}
	} else if defaults != nil {
		if defaults.Spec.DefaultCPU != "" {
			cpuReq = defaults.Spec.DefaultCPU
		}
		if defaults.Spec.DefaultMemory != "" {
			memReq = defaults.Spec.DefaultMemory
		}
	}
	if draft.Spec.Persistence != nil && draft.Spec.Persistence.Size != "" {
		stgReq = draft.Spec.Persistence.Size
	} else if defaults != nil && defaults.Spec.DefaultStorage != "" {
		stgReq = defaults.Spec.DefaultStorage
	}

	cpu, errCPU := resource.ParseQuantity(cpuReq)
	mem, errMem := resource.ParseQuantity(memReq)
	stg, errStg := resource.ParseQuantity(stgReq)

	if errCPU != nil {
		return Check{ID: "quota", Status: "fail", Message: "invalid cpu quantity: " + cpuReq}
	}
	if errMem != nil {
		return Check{ID: "quota", Status: "fail", Message: "invalid memory quantity: " + memReq}
	}
	if errStg != nil {
		return Check{ID: "quota", Status: "fail", Message: "invalid storage quantity: " + stgReq}
	}

	for _, q := range quotas {
		dimensions := []struct {
			used *resource.Quantity
			hard *resource.Quantity
			name string
			req  resource.Quantity
		}{
			{q.Status.Used.Name(corev1.ResourceRequestsCPU, resource.DecimalSI), q.Status.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI), "cpu", cpu},
			{q.Status.Used.Name(corev1.ResourceRequestsMemory, resource.DecimalSI), q.Status.Hard.Name(corev1.ResourceRequestsMemory, resource.DecimalSI), "memory", mem},
			{q.Status.Used.Name(corev1.ResourceRequestsStorage, resource.DecimalSI), q.Status.Hard.Name(corev1.ResourceRequestsStorage, resource.DecimalSI), "storage", stg},
		}

		for _, d := range dimensions {
			if d.used == nil || d.hard == nil {
				continue
			}
			remaining := d.hard.DeepCopy()
			remaining.Sub(*d.used)
			if d.req.Cmp(remaining) > 0 {
				return Check{
					ID:      "quota",
					Status:  "fail",
					Message: "requested " + d.name + " " + d.req.String() + " exceeds remaining " + remaining.String(),
				}
			}
		}
	}

	return Check{ID: "quota", Status: "pass", Message: "quota check passed"}
}

func checkIngressHost(ctx context.Context, deps Deps, draft *v1alpha1.Controller, defaults *v1alpha1.ProvisioningDefaults) Check {
	host := ""
	if draft.Spec.IngressSpec != nil {
		host = draft.Spec.IngressSpec.Host
	}
	// Resolve host from rootDomain when in subdomain mode and no explicit host.
	if host == "" && defaults != nil && defaults.Spec.RootDomain != "" {
		isSubdomain := draft.Spec.IngressSpec == nil || draft.Spec.IngressSpec.Mode == "" || draft.Spec.IngressSpec.Mode == "subdomain"
		if isSubdomain {
			host = draft.Name + "." + defaults.Spec.RootDomain
		}
	}

	if host == "" {
		return Check{ID: "ingress-host", Status: "pass", Message: "no host to check"}
	}

	hosts, err := deps.ListIngressHosts(ctx)
	if err != nil {
		return Check{
			ID:      "ingress-host",
			Status:  "warn",
			Message: "unable to list ingress hosts: " + err.Error(),
		}
	}

	if claimants, ok := hosts[host]; ok && len(claimants) > 0 {
		return Check{
			ID:      "ingress-host",
			Status:  "warn",
			Message: "host " + host + " is already claimed by " + claimants[0],
		}
	}

	return Check{ID: "ingress-host", Status: "pass", Message: "host " + host + " is available"}
}

func checkRBAC(ctx context.Context, deps Deps, draft *v1alpha1.Controller, opts Options) Check {
	if draft.Spec.RBACSpec == nil || len(draft.Spec.RBACSpec.Groups) == 0 {
		return Check{ID: "rbac", Status: "pass", Message: "no RBAC groups configured"}
	}

	roles, err := crdstore.List[v1alpha1.VarroaRole](ctx, deps, "", "")
	if err != nil {
		return Check{
			ID:      "rbac",
			Status:  "warn",
			Message: "unable to list roles: " + err.Error(),
		}
	}

	validRoles := make(map[string]bool)
	for _, r := range roles {
		if r.Labels != nil && r.Labels[v1alpha1.LabelBuiltin] == "true" {
			validRoles[r.Name] = true
		}
	}

	for _, g := range draft.Spec.RBACSpec.Groups {
		if !validRoles[g.Role] {
			return Check{
				ID:      "rbac",
				Status:  "fail",
				Message: "role " + g.Role + " is not a known built-in role",
			}
		}
	}

	groups, err := crdstore.List[v1alpha1.Group](ctx, deps, "", "")
	if err != nil {
		return Check{ID: "rbac", Status: "warn", Message: "unable to list groups: " + err.Error()}
	}

	users, err := crdstore.List[v1alpha1.User](ctx, deps, opts.OperatorNamespace, "")
	if err != nil {
		return Check{ID: "rbac", Status: "warn", Message: "unable to list users: " + err.Error()}
	}

	localGroups := make(map[string]int)
	for _, g := range groups {
		localGroups[g.Name] = len(g.Spec.Members)
	}

	idpGroups := make(map[string]int)
	for _, u := range users {
		for _, g := range u.Status.ObservedGroups {
			idpGroups[g]++
		}
	}

	for _, g := range draft.Spec.RBACSpec.Groups {
		local := localGroups[g.Name]
		idp := idpGroups[g.Name]
		if local == 0 && idp == 0 {
			return Check{
				ID:      "rbac",
				Status:  "warn",
				Message: "group " + g.Name + " has no members seen yet",
			}
		}
	}

	return Check{ID: "rbac", Status: "pass", Message: "RBAC groups are valid"}
}
