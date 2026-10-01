package main

import (
	"errors"
	"os"

	"github.com/varroaci/varroa-jenkins/internal/controller"
)

// artifactSTSConfigFromEnv reads the four VARROA_ARTIFACT_STS_* values. STS
// mode and the static VARROA_ARTIFACT_SECRET base are exclusive: one
// install issues its cell artifact credentials one way only.
func artifactSTSConfigFromEnv(artifactSecretBase string) (controller.ArtifactSTSConfig, error) {
	cfg, err := controller.ParseArtifactSTSConfig(
		os.Getenv("VARROA_ARTIFACT_STS_ISSUER_SECRET"),
		os.Getenv("VARROA_ARTIFACT_STS_ROLE_ARN"),
		os.Getenv("VARROA_ARTIFACT_STS_ENDPOINT"),
		os.Getenv("VARROA_ARTIFACT_STS_DURATION"),
	)
	if err != nil {
		return controller.ArtifactSTSConfig{}, err
	}
	if cfg.Enabled() && artifactSecretBase != "" {
		return controller.ArtifactSTSConfig{}, errors.New("VARROA_ARTIFACT_STS_ISSUER_SECRET and VARROA_ARTIFACT_SECRET are exclusive; set one")
	}
	return cfg, nil
}
