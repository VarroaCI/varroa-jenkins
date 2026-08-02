package bundle

import (
	"testing"
)

func TestFindUnresolvedVariables(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		userVars map[string]string
		want     []string
	}{
		{
			name:     "empty content",
			content:  "",
			userVars: nil,
			want:     []string{},
		},
		{
			name:     "no placeholders",
			content:  "jenkins:\n  key: value\n",
			userVars: nil,
			want:     []string{},
		},
		{
			name:    "one missing var",
			content: "${artifactory_url}/path",
			want:    []string{"artifactory_url"},
		},
		{
			name:    "injected-only vars are excluded",
			content: "${varroa_controller_name}/${varroa_controller_namespace}",
			want:    []string{},
		},
		{
			name:     "supplied vars are excluded",
			content:  "${my_var}",
			userVars: map[string]string{"my_var": "resolved"},
			want:     []string{},
		},
		{
			name:     "mix of supplied, injected, and unresolved",
			content:  "${supplied} ${varroa_controller_name} ${missing}",
			userVars: map[string]string{"supplied": "ok"},
			want:     []string{"missing"},
		},
		{
			name:    "escaped placeholders skipped",
			content: "^${escaped} ${real}",
			want:    []string{"real"},
		},
		{
			name:    "multiple unresolved sorted",
			content: "${z_var} ${a_var} ${m_var}",
			want:    []string{"a_var", "m_var", "z_var"},
		},
		{
			name:    "duplicates deduped",
			content: "${dup} ${dup}",
			want:    []string{"dup"},
		},
		{
			name:    "varroa_oidc_client_secret is now unresolved",
			content: "${varroa_oidc_client_secret}",
			want:    []string{"varroa_oidc_client_secret"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindUnresolvedVariables(tt.content, tt.userVars)
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got %v, want %v", got, tt.want)
					return
				}
			}
		})
	}
}
