package bundle

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInjectLocationURL(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		out, overrode, err := InjectLocationURL("", "https://varroa.example.com/jenkins/team-a/ci/")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if overrode {
			t.Error("expected overrode=false for empty input")
		}
		if out == "" {
			t.Fatal("expected non-empty output")
		}
	})

	t.Run("empty input produces minimal location", func(t *testing.T) {
		url := "https://ci.example.com/"
		out, overrode, err := InjectLocationURL("", url)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if overrode {
			t.Error("expected overrode=false for empty input")
		}
		result := make(map[string]interface{})
		if err := yaml.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
		unclassified, ok := result["unclassified"].(map[string]interface{})
		if !ok {
			t.Fatal("unclassified not present")
		}
		if len(unclassified) != 1 {
			t.Errorf("expected exactly 1 key under unclassified (location), got %d: %v", len(unclassified), unclassified)
		}
		location, ok := unclassified["location"].(map[string]interface{})
		if !ok {
			t.Fatal("unclassified.location not present")
		}
		if len(location) != 1 {
			t.Errorf("expected exactly 1 key under location (url), got %d: %v", len(location), location)
		}
		if location["url"] != url {
			t.Errorf("location.url = %v, want %q", location["url"], url)
		}
	})

	t.Run("absent unclassified", func(t *testing.T) {
		in := "jenkins:\n  security:\n    remotingCLI: false\n"
		out, overrode, err := InjectLocationURL(in, "https://varroa.example.com/jenkins/team-a/ci/")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if overrode {
			t.Error("expected overrode=false when no location was present")
		}
		if out == "" {
			t.Fatal("expected non-empty output")
		}
	})

	t.Run("existing different url", func(t *testing.T) {
		in := "unclassified:\n  location:\n    url: http://internal:8080/jenkins/\n"
		out, overrode, err := InjectLocationURL(in, "https://varroa.example.com/jenkins/team-a/ci/")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !overrode {
			t.Error("expected overrode=true when existing url differs")
		}
		if out == "" {
			t.Fatal("expected non-empty output")
		}
	})

	t.Run("existing same url", func(t *testing.T) {
		in := "unclassified:\n  location:\n    url: https://varroa.example.com/jenkins/team-a/ci/\n"
		out, overrode, err := InjectLocationURL(in, "https://varroa.example.com/jenkins/team-a/ci/")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if overrode {
			t.Error("expected overrode=false when existing url is same")
		}
		if out == "" {
			t.Fatal("expected non-empty output")
		}
	})

	t.Run("preserves unrelated keys", func(t *testing.T) {
		in := "unclassified:\n  location:\n    url: http://old:8080/\n  otherSetting: true\njenkins:\n  security:\n    remotingCLI: false\n"
		out, overrode, err := InjectLocationURL(in, "https://varroa.example.com/jenkins/team-a/ci/")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !overrode {
			t.Error("expected overrode=true")
		}
		// Re-parse and verify round-trip preserves unrelated keys.
		result := make(map[string]interface{})
		if err := yaml.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
		unclassified, ok := result["unclassified"].(map[string]interface{})
		if !ok {
			t.Fatal("unclassified not present after round trip")
		}
		if unclassified["otherSetting"] != true {
			t.Error("otherSetting was not preserved")
		}
		jenkins, ok := result["jenkins"].(map[string]interface{})
		if !ok {
			t.Fatal("jenkins not present after round trip")
		}
		security, ok := jenkins["security"].(map[string]interface{})
		if !ok {
			t.Fatal("jenkins.security not present after round trip")
		}
		if security["remotingCLI"] != false {
			t.Error("remotingCLI was not preserved")
		}
	})
}
