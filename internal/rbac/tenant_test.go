package rbac

import "testing"

// TestParseTenant covers the complete value set: a name, a two-part path,
// and the malformed values "a/", "/b", "a/b/c", and the empty string.
func TestParseTenant(t *testing.T) {
	tests := []struct {
		name    string
		tenant  string
		wantErr bool
		want    ParsedTenant
	}{
		{
			name:   "tenant name",
			tenant: "payments",
			want: ParsedTenant{
				Kind:   TenantTenant,
				Tenant: "payments",
				Folder: "payments",
			},
		},
		{
			name:   "controller path",
			tenant: "jenkins-prod/ci-1",
			want: ParsedTenant{
				Kind:       TenantController,
				Namespace:  "jenkins-prod",
				Controller: "ci-1",
				Folder:     "",
			},
		},
		{name: "trailing slash", tenant: "a/", wantErr: true},
		{name: "leading slash", tenant: "/b", wantErr: true},
		{name: "three parts", tenant: "a/b/c", wantErr: true},
		{name: "empty string", tenant: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseTenant(tt.tenant)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseTenant(%q) = %+v, want error", tt.tenant, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTenant(%q) returned error: %v", tt.tenant, err)
			}
			if got != tt.want {
				t.Fatalf("ParseTenant(%q) = %+v, want %+v", tt.tenant, got, tt.want)
			}
		})
	}
}

// TestParseTenantNeedsNoState calls the free function directly, with no
// Resolver and no informer, proving ParseTenant performs no lookup.
func TestParseTenantNeedsNoState(t *testing.T) {
	got, err := ParseTenant("tenant-a")
	if err != nil {
		t.Fatalf("ParseTenant returned error with no Resolver or informer: %v", err)
	}
	if got.Kind != TenantTenant || got.Tenant != "tenant-a" {
		t.Fatalf("ParseTenant(%q) = %+v, want TenantTenant %q", "tenant-a", got, "tenant-a")
	}
}
