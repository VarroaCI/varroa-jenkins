package forgeurl

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRelayBody(t *testing.T) {
	if err := ValidateRelayBody(strings.Repeat("x", MaxRelayBodyBytes)); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if err := ValidateRelayBody(strings.Repeat("x", MaxRelayBodyBytes+1)); err == nil {
		t.Fatal("over the limit: want refusal")
	}
}

func TestValidateCommitMessage(t *testing.T) {
	ok := []string{"Add a file", strings.Repeat("x", MaxCommitMessageBytes)}
	for _, m := range ok {
		if err := ValidateCommitMessage(m); err != nil {
			t.Errorf("%.20q: %v", m, err)
		}
	}
	bad := map[string]string{
		"empty":    "",
		"not utf8": "\xff",
		"nul":      "a\x00b",
		"too long": strings.Repeat("x", MaxCommitMessageBytes+1),
	}
	for name, m := range bad {
		err := ValidateCommitMessage(m)
		var re *RuleError
		if !errors.As(err, &re) || re.Arg != "commitMessage" {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
