package v1

import (
	"errors"
	"testing"
)

// TestValidateRejections walks the refusals of Validate.
func TestValidateRejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec Spec
		want error
	}{
		{"blank image", Spec{Image: "  "}, ErrEmptyImage},
		{"github token", Spec{Image: "x", Env: map[string]string{"A": "gh" + "p_" + "aaaaaaaaaaaaaaaaaaaaaaaa"}}, ErrCredentialEnv},
		{"aws key", Spec{Image: "x", Env: map[string]string{"A": "AKIA" + "ABCDEFGHIJKLMNOP"}}, ErrCredentialEnv},
		{"pem block", Spec{Image: "x", Env: map[string]string{"A": "-----BEGIN " + "RSA PRIVATE KEY-----"}}, ErrCredentialEnv},
		{"jwt", Spec{Image: "x", Env: map[string]string{"A": "eyJ" + "hbGciOi.payload.sig"}}, ErrCredentialEnv},
		{"bearer", Spec{Image: "x", Env: map[string]string{"A": "Bearer " + "abc"}}, ErrCredentialEnv},
		{"rule without target", Spec{Image: "x", Egress: []Rule{{Port: 443}}}, ErrRuleTarget},
	}
	for _, c := range cases {
		err := c.spec.Validate()
		if !errors.Is(err, c.want) || !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("%s: Validate = %v, want %v", c.name, err, c.want)
		}
	}

	ok := Spec{Image: "x", Env: map[string]string{"MODE": "fast"}, Egress: []Rule{{Host: "a.invalid", Port: 443}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid spec refused: %v", err)
	}
}
