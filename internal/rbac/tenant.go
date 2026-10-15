package rbac

import "fmt"

// TenantKind classifies a tenant name.
type TenantKind int

const (
	// TenantTeam names a Team placed on a cell.
	TenantTeam TenantKind = iota
	// TenantController names one controller as "<namespace>/<name>".
	TenantController
)

// ParsedTenant is the classification of a tenant value. It carries no cluster identity.
type ParsedTenant struct {
	Kind       TenantKind
	Team       string // set when Kind is TenantTeam
	Namespace  string // set when Kind is TenantController
	Controller string // set when Kind is TenantController
	Folder     string // the Jenkins folder prefix: Team for TenantTeam, "" for TenantController
}

// ParseTenant classifies a stored or resolved tenant value. It performs no lookup.
//
// A value with no "/" is a TenantTeam. A value with exactly one "/", and a
// non-empty part on each side, is a TenantController. Every other value,
// including the empty string, returns an error.
func ParseTenant(tenant string) (ParsedTenant, error) {
	if tenant == "" {
		return ParsedTenant{}, fmt.Errorf("rbac: tenant value is empty")
	}

	idx := -1
	for i := 0; i < len(tenant); i++ {
		if tenant[i] == '/' {
			idx = i
			break
		}
	}

	if idx == -1 {
		return ParsedTenant{
			Kind:   TenantTeam,
			Team:   tenant,
			Folder: tenant,
		}, nil
	}

	namespace := tenant[:idx]
	rest := tenant[idx+1:]
	// A second "/" in rest, or an empty side, makes the value malformed.
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return ParsedTenant{}, fmt.Errorf("rbac: malformed tenant value %q", tenant)
		}
	}
	if namespace == "" || rest == "" {
		return ParsedTenant{}, fmt.Errorf("rbac: malformed tenant value %q", tenant)
	}

	return ParsedTenant{
		Kind:       TenantController,
		Namespace:  namespace,
		Controller: rest,
		Folder:     "",
	}, nil
}
