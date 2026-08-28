package apikey

import (
	"strings"
	"testing"
)

func TestGenerateAndParse(t *testing.T) {
	prefix, secret, token, err := Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if !strings.HasPrefix(token, "vk_") {
		t.Errorf("token missing vk_ prefix: %s", token)
	}
	if len(prefix) != prefixEncLen {
		t.Errorf("prefix length = %d, want %d", len(prefix), prefixEncLen)
	}
	if len(secret) != secretEncLen {
		t.Errorf("secret length = %d, want %d", len(secret), secretEncLen)
	}

	gotPrefix, gotSecret, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse() error: %v", err)
	}
	if gotPrefix != prefix {
		t.Errorf("Parse prefix = %s, want %s", gotPrefix, prefix)
	}
	if gotSecret != secret {
		t.Errorf("Parse secret = %s, want %s", gotSecret, secret)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"v_abc.def",
		"vk_",
		"vk_abc",
		"vk_abc.def.ghi",
		"vk_short.secret",
		"vk_abcdefghijklmnop.longsecret",
	}
	for _, tc := range cases {
		_, _, err := Parse(tc)
		if err == nil {
			t.Errorf("expected error for token %q", tc)
		}
	}
}
