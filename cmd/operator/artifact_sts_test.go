package main

import (
	"strings"
	"testing"
	"time"
)

func TestArtifactSTSConfigFromEnv(t *testing.T) {
	t.Run("unset keeps static mode", func(t *testing.T) {
		cfg, err := artifactSTSConfigFromEnv("varroa-cell-artifacts")
		if err != nil || cfg.Enabled() {
			t.Fatalf("cfg=%+v err=%v, want disabled and no error", cfg, err)
		}
	})
	t.Run("defaults the duration", func(t *testing.T) {
		t.Setenv("VARROA_ARTIFACT_STS_ISSUER_SECRET", "varroa-artifact-issuer")
		t.Setenv("VARROA_ARTIFACT_STS_ROLE_ARN", "arn:minio:iam:::role/varroa-artifacts")
		cfg, err := artifactSTSConfigFromEnv("")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !cfg.Enabled() || cfg.Duration != time.Hour {
			t.Fatalf("cfg = %+v, want enabled with a 1h duration", cfg)
		}
	})
	t.Run("refuses a duration under the floor", func(t *testing.T) {
		t.Setenv("VARROA_ARTIFACT_STS_ISSUER_SECRET", "varroa-artifact-issuer")
		t.Setenv("VARROA_ARTIFACT_STS_ROLE_ARN", "arn:minio:iam:::role/varroa-artifacts")
		t.Setenv("VARROA_ARTIFACT_STS_DURATION", "10m")
		if _, err := artifactSTSConfigFromEnv(""); err == nil {
			t.Fatal("expected an error for a 10m duration")
		}
	})
	t.Run("requires the role ARN", func(t *testing.T) {
		t.Setenv("VARROA_ARTIFACT_STS_ISSUER_SECRET", "varroa-artifact-issuer")
		if _, err := artifactSTSConfigFromEnv(""); err == nil || !strings.Contains(err.Error(), "ROLE_ARN") {
			t.Fatalf("err = %v, want a ROLE_ARN error", err)
		}
	})
	t.Run("refuses STS beside the static base", func(t *testing.T) {
		t.Setenv("VARROA_ARTIFACT_STS_ISSUER_SECRET", "varroa-artifact-issuer")
		t.Setenv("VARROA_ARTIFACT_STS_ROLE_ARN", "arn:minio:iam:::role/varroa-artifacts")
		if _, err := artifactSTSConfigFromEnv("varroa-cell-artifacts"); err == nil || !strings.Contains(err.Error(), "exclusive") {
			t.Fatalf("err = %v, want an exclusive error", err)
		}
	})
}
